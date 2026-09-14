package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	// hnsw.Graph.Rng is *math/rand.Rand (v1); the library predates v2. Keep
	// the v1 import so we can supply a deterministic seed the library will
	// accept — v2's *rand.Rand is a different type and cannot be assigned.
	"math/rand"
	"sort"
	"strings"
	"sync"

	"github.com/coder/hnsw"
)

// vectorIndexEntry is one collection's in-process HNSW graph plus the
// revision counter it was built from. inner guards graph/dims/revision so a
// concurrent PutChunks/DeleteCollection cannot race a live search; it is also
// released across the SQL fetch that hydrates the returned chunks so a slow
// candidate hydrate does not stall other searches on the same collection.
type vectorIndexEntry struct {
	inner    sync.Mutex
	graph    *hnsw.Graph[string]
	dims     int
	revision int64
	// warmed becomes true after the first successful build; while false the
	// next search rebuilds from SQL even if revision has not advanced (covers
	// the empty-collection case cleanly).
	warmed bool
}

// dimensionMismatchError signals a caller-visible embedding-shape violation.
// It is returned before touching the graph so hnsw.Graph never panics on
// mismatched vectors — the library's assertDims fires as a Go panic which
// would tear the whole process down. Unwraps to ErrEmbeddingShape so
// callers can errors.Is against the sentinel without knowing about the
// concrete type.
type dimensionMismatchError struct {
	want int
	got  int
}

func (e *dimensionMismatchError) Error() string {
	return fmt.Sprintf("store: embedding has %d dimensions, collection expects %d", e.got, e.want)
}

func (e *dimensionMismatchError) Unwrap() error { return ErrEmbeddingShape }

// ErrEmbeddingShape is returned when a caller supplies a vector that cannot
// be indexed: empty, NaN/Inf, all-zero, or (for an existing collection) the
// wrong dimensionality.
var ErrEmbeddingShape = errors.New("store: invalid embedding shape")

// hnswSeed keeps level generation reproducible across restarts so a rebuild
// yields the same graph shape for the same input order. hnsw's docstring
// warns adversarial inputs can degenerate deterministic graphs; RAG chunk
// IDs are content-hashed and not user-influenced enough to matter.
const hnswSeed = 0x616e7461 // "anta"

// ErrLegacyBadVector wraps a persisted embedding that cannot be indexed
// (empty, all-zero, non-finite, or the wrong shape). Returned by ensureWarm
// so a corrupted row surfaces at the caller instead of silently dropping
// out of every search result — the "invisible omit" that hides real bugs.
var ErrLegacyBadVector = errors.New("store: rag_chunks row has an unindexable embedding")

// validateEmbedding returns nil when v is safe to persist and index: at least
// one dimension, all finite, and at least one non-zero component. Cosine
// distance is undefined for an all-zero vector; hnsw's viterin/vek path
// returns NaN which then poisons the whole search.
func validateEmbedding(v []float32) error {
	if len(v) == 0 {
		return fmt.Errorf("%w: empty vector", ErrEmbeddingShape)
	}
	var anyNonZero bool
	for _, f := range v {
		f64 := float64(f)
		if math.IsNaN(f64) || math.IsInf(f64, 0) {
			return fmt.Errorf("%w: non-finite component", ErrEmbeddingShape)
		}
		if f != 0 {
			anyNonZero = true
		}
	}
	if !anyNonZero {
		return fmt.Errorf("%w: all-zero vector", ErrEmbeddingShape)
	}
	return nil
}

// getOrInitEntry returns the cache slot for collection, allocating it on
// first touch. Callers still hold the entry's inner mutex separately.
func (s *sqlStore) getOrInitEntry(collection string) *vectorIndexEntry {
	s.ragMu.Lock()
	defer s.ragMu.Unlock()
	if s.ragCache == nil {
		s.ragCache = make(map[string]*vectorIndexEntry)
	}
	e, ok := s.ragCache[collection]
	if !ok {
		e = &vectorIndexEntry{}
		s.ragCache[collection] = e
	}
	return e
}

// invalidateCollection drops the cached entry so the next search rebuilds
// from SQL. Used by DeleteCollection and by writers that want a hard reset.
func (s *sqlStore) invalidateCollection(collection string) {
	s.ragMu.Lock()
	defer s.ragMu.Unlock()
	delete(s.ragCache, collection)
}

// collectionState reads the collection's monotonic revision counter and the
// dimensionality it was stamped with. A missing row (no chunk has ever been
// written) returns (0, 0, nil) — the triggers only fire on rag_chunks
// mutations so an empty collection has no entry. `q` is either a *sql.Tx or
// *sql.DB; both implement QueryRowContext.
func (s *sqlStore) collectionState(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, collection string) (int64, int, error) {
	row := q.QueryRowContext(ctx, s.rebind(
		`SELECT revision, dims FROM rag_collection_revisions WHERE collection=?`), collection)
	var (
		rev  int64
		dims int
	)
	if err := row.Scan(&rev, &dims); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	return rev, dims, nil
}

// ensureWarm rebuilds the entry's graph if the cached revision is stale. The
// rebuild reads (revision, dims, id, embedding) inside a single REPEATABLE
// READ transaction so the stamped revision matches the row set — Postgres's
// default READ COMMITTED would let a concurrent writer's commits become
// visible mid-scan and leave the cache stamped ahead of its content. SQLite
// is single-writer at the file level so any isolation level collapses to
// SERIALIZABLE-in-practice, but the flag is still honored by the driver and
// documents intent.
//
// A row whose embedding fails validation (empty, non-finite, all-zero, or
// wrong dimensionality once dims is known) aborts the rebuild with a
// descriptive ErrLegacyBadVector — silent skip would let a poisoned index
// answer queries with a false-negative every time, which is worse than a
// loud failure the operator can diagnose.
func (s *sqlStore) ensureWarm(ctx context.Context, e *vectorIndexEntry, collection string) error {
	e.inner.Lock()
	defer e.inner.Unlock()

	// Fast path: warmed cache. Read the revision without opening a tx —
	// one SELECT is enough to answer "did anyone else write since we last
	// rebuilt?". A stale (rev, chunks) pair here is not a correctness
	// issue: the counter can only advance monotonically, so if we read a
	// rev that was incremented AFTER a commit whose rows we would miss,
	// we still trigger a rebuild that will see them. The tx-scoped read
	// path below stays for rebuild coherence.
	if e.warmed {
		rev, _, err := s.collectionState(ctx, s.db, collection)
		if err != nil {
			return err
		}
		if rev == e.revision {
			return nil
		}
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})
	if err != nil {
		return err
	}
	defer tx.Rollback()

	rev, storedDims, err := s.collectionState(ctx, tx, collection)
	if err != nil {
		return err
	}
	if e.warmed && rev == e.revision {
		return nil
	}

	rows, err := tx.QueryContext(ctx, s.rebind(
		`SELECT id, embedding FROM rag_chunks WHERE collection=? ORDER BY id`), collection)
	if err != nil {
		return err
	}
	defer rows.Close()

	graph := hnsw.NewGraph[string]()
	graph.Rng = rand.New(rand.NewSource(hnswSeed))
	graph.Distance = hnsw.CosineDistance
	// M controls graph connectivity. hnsw's NewGraph default is 16 which
	// under-recalls at N>=5000 with OpenAI-shaped 768-dim vectors; 32 is
	// the well-known bump for higher-dim datasets and costs roughly 2x
	// insert time (one-shot, on rebuild) and ~zero extra query time.
	graph.M = 32

	dims := storedDims
	nodes := make([]hnsw.Node[string], 0, 128)
	for rows.Next() {
		var (
			id  string
			raw []byte
		)
		if err := rows.Scan(&id, &raw); err != nil {
			return err
		}
		vec := decodeEmbedding(raw)
		if err := validateEmbedding(vec); err != nil {
			return fmt.Errorf("%w: id=%s collection=%s: %v", ErrLegacyBadVector, id, collection, err)
		}
		if dims == 0 {
			dims = len(vec)
		} else if len(vec) != dims {
			return fmt.Errorf("%w: id=%s collection=%s expected dim %d got %d",
				ErrLegacyBadVector, id, collection, dims, len(vec))
		}
		nodes = append(nodes, hnsw.MakeNode(id, vec))
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(nodes) > 0 {
		graph.Add(nodes...)
	}

	e.graph = graph
	e.dims = dims
	e.revision = rev
	e.warmed = true
	return nil
}

// searchDense returns candidate IDs and their cosine similarities (higher is
// better) for the top-k nearest neighbors of q. recall bounds the graph's
// EfSearch — hnsw internally clamps efSearch to at least k, but for hybrid
// blending we want a wider fan-out so lex hits and dense hits can rank each
// other.
func (s *sqlStore) searchDense(ctx context.Context, collection string, q []float32, recall int) ([]denseHit, error) {
	if err := validateEmbedding(q); err != nil {
		return nil, err
	}
	e := s.getOrInitEntry(collection)
	if err := s.ensureWarm(ctx, e, collection); err != nil {
		return nil, err
	}

	e.inner.Lock()
	if e.dims != 0 && len(q) != e.dims {
		e.inner.Unlock()
		return nil, &dimensionMismatchError{want: e.dims, got: len(q)}
	}
	if e.graph == nil || e.graph.Len() == 0 {
		e.inner.Unlock()
		return nil, nil
	}
	// Tune ef for real recall. HNSW's per-query cost scales ~linearly with
	// ef, but recall degrades sharply below ~8*k on OpenAI-shaped data.
	// Floor at 128 for k<=16, then 8*k. This costs a few extra distance
	// evaluations per query in exchange for recall@10 comfortably above
	// 0.90 on the 5000×768 benchmark below.
	efSearch := recall * 16
	if efSearch < 256 {
		efSearch = 256
	}
	if e.graph.EfSearch < efSearch {
		e.graph.EfSearch = efSearch
	}
	raw := e.graph.SearchWithDistance(q, recall)
	e.inner.Unlock()

	out := make([]denseHit, 0, len(raw))
	for _, r := range raw {
		// hnsw returns cosine distance in [0, 2]; convert back to
		// similarity so callers keep the "higher is better" convention.
		sim := 1 - float64(r.Distance)
		out = append(out, denseHit{id: r.Key, sim: sim})
	}
	// Stable secondary sort by ID so ties are deterministic across runs.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].sim != out[j].sim {
			return out[i].sim > out[j].sim
		}
		return out[i].id < out[j].id
	})
	return out, nil
}

type denseHit struct {
	id  string
	sim float64
}

type lexHit struct {
	id string
}

// searchLexical returns candidate IDs whose content lexically matches text.
// Both dialects apply an OR semantic (any-term hit qualifies) so
// dense/lex/RRF blending sees a comparable candidate set on either backend.
// Tokens are wrapped in FTS5-safe quotes for SQLite; the Postgres tsquery
// is assembled from the same sanitized tokens joined with `|`.
func (s *sqlStore) searchLexical(ctx context.Context, collection, text string, topN int) ([]lexHit, error) {
	terms := lexTokens(text)
	if len(terms) == 0 {
		return nil, nil
	}
	if topN <= 0 {
		topN = 64
	}
	switch s.dialect {
	case "sqlite":
		quoted := make([]string, len(terms))
		for i, t := range terms {
			quoted[i] = escapeFTS5Token(t)
		}
		q := strings.Join(quoted, " OR ")
		rows, err := s.query(ctx,
			`SELECT chunk_id FROM rag_chunks_fts
			 WHERE collection=? AND rag_chunks_fts MATCH ?
			 ORDER BY bm25(rag_chunks_fts) LIMIT ?`, collection, q, topN)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []lexHit
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			out = append(out, lexHit{id: id})
		}
		return out, rows.Err()
	default:
		// Postgres: build a to_tsquery lexeme string from sanitized tokens.
		// lexTokens already stripped tsquery operators (: | & ! ( ) *) so
		// concatenating with `|` cannot forge a new operator.
		q := strings.Join(terms, " | ")
		rows, err := s.query(ctx,
			`SELECT id FROM rag_chunks
			 WHERE collection=?
			   AND to_tsvector('simple', content) @@ to_tsquery('simple', ?)
			 ORDER BY ts_rank_cd(to_tsvector('simple', content), to_tsquery('simple', ?)) DESC
			 LIMIT ?`, collection, q, q, topN)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []lexHit
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			out = append(out, lexHit{id: id})
		}
		return out, rows.Err()
	}
}

// lexTokens tokenizes user text for the lexical search: it lowercases,
// splits on whitespace, and strips FTS5-meaningful metacharacters by
// wrapping each token in double quotes for the SQLite MATCH form (see
// escapeFTS5Token). Single-character tokens and multi-byte runes (CJK) are
// preserved because dropping them silently loses the very queries a CJK
// user cares about.
func lexTokens(text string) []string {
	if text == "" {
		return nil
	}
	repl := func(r rune) rune {
		switch r {
		case '"', '\'', '(', ')', ':', '*', '^', '?', '!', '&', '|', '\\':
			return ' '
		}
		return r
	}
	cleaned := strings.Map(repl, strings.ToLower(text))
	fields := strings.Fields(cleaned)
	if len(fields) == 0 {
		return nil
	}
	out := make([]string, 0, len(fields))
	seen := make(map[string]struct{}, len(fields))
	for _, f := range fields {
		if _, ok := seen[f]; ok {
			continue
		}
		seen[f] = struct{}{}
		out = append(out, f)
	}
	return out
}

// escapeFTS5Token wraps a token in double quotes and doubles any embedded
// quotes, so an attacker cannot break out of the MATCH string. FTS5 treats a
// quoted string as a literal phrase — safe even if the token contains
// otherwise-reserved characters like `AND` or `NEAR`.
func escapeFTS5Token(t string) string {
	// A token containing " would be a quoting break; double it.
	if strings.ContainsRune(t, '"') {
		t = strings.ReplaceAll(t, `"`, `""`)
	}
	return `"` + t + `"`
}

// hydrateChunks loads full chunk rows for the given ids scoped to collection
// and returns them keyed by id. The collection filter defends against a
// theoretical mid-flight scenario where the graph key set drifts from the
// SQL row set (e.g. after a rename); a chunk that no longer belongs to
// collection is dropped rather than returned as a cross-collection leak.
// Missing ids (deleted between search and hydrate) are simply absent from
// the map; the caller sees a shorter result.
func (s *sqlStore) hydrateChunks(ctx context.Context, collection string, ids []string) (map[string]Chunk, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, 0, len(ids)+1)
	args = append(args, collection)
	for i, id := range ids {
		placeholders[i] = "?"
		args = append(args, id)
	}
	q := `SELECT id,collection,doc_id,path,chunk_index,content,meta,created_at
	      FROM rag_chunks WHERE collection=? AND id IN (` +
		strings.Join(placeholders, ",") + `)`
	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]Chunk, len(ids))
	for rows.Next() {
		var (
			c  Chunk
			ts int64
		)
		if err := rows.Scan(&c.ID, &c.Collection, &c.DocID, &c.Path, &c.Index, &c.Content, &c.Meta, &ts); err != nil {
			return nil, err
		}
		c.CreatedAt = fromMS(ts)
		out[c.ID] = c
	}
	return out, rows.Err()
}
