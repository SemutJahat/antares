package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// unitVector returns a deterministic pseudo-random unit vector of dim
// dimensions from the supplied PCG source. Cosine over unit vectors is a
// clean fixture: identical vectors score 1.0 exactly, orthogonal 0.0.
func unitVector(rng *rand.Rand, dim int) []float32 {
	v := make([]float32, dim)
	var norm float64
	for i := range v {
		v[i] = float32(rng.NormFloat64())
		norm += float64(v[i]) * float64(v[i])
	}
	inv := 1.0 / math.Sqrt(norm)
	for i := range v {
		v[i] = float32(float64(v[i]) * inv)
	}
	return v
}

// TestVectorIndexExactNearest verifies that on a tiny collection the HNSW
// path returns the strictly closest chunk — the small-collection recall bar
// that a full brute-force scan would trivially hit.
func TestVectorIndexExactNearest(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	chunks := []Chunk{
		{ID: "c1", Collection: "proj", Content: "http server in go", Embedding: []float32{1, 0, 0}},
		{ID: "c2", Collection: "proj", Content: "react component", Embedding: []float32{0, 1, 0}},
		{ID: "c3", Collection: "proj", Content: "postgres tuning", Embedding: []float32{0, 0, 1}},
	}
	if err := s.PutChunks(ctx, chunks); err != nil {
		t.Fatalf("put: %v", err)
	}
	res, scores, err := s.SearchChunks(ctx, "proj", []float32{0, 1, 0}, "", 1, false)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res) != 1 || res[0].ID != "c2" {
		t.Fatalf("wrong nearest: %+v", res)
	}
	if scores[0] < 0.999 {
		t.Fatalf("cosine of identical unit vectors should be ~1, got %v", scores[0])
	}
}

// TestVectorIndexWarmMutationVisibility exercises the cache invalidation
// contract: after an initial search warms the graph, a subsequent PutChunks
// must be visible to the very next search — the revision counter bumped by
// the SQL trigger drives the rebuild.
func TestVectorIndexWarmMutationVisibility(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	seed := []Chunk{{ID: "c1", Collection: "proj", Content: "alpha", Embedding: []float32{1, 0, 0}}}
	if err := s.PutChunks(ctx, seed); err != nil {
		t.Fatalf("put: %v", err)
	}
	// Warm the cache.
	if _, _, err := s.SearchChunks(ctx, "proj", []float32{1, 0, 0}, "", 1, false); err != nil {
		t.Fatalf("warm: %v", err)
	}

	// Introduce a new chunk that is a better match for the query.
	added := []Chunk{{ID: "c2", Collection: "proj", Content: "beta", Embedding: []float32{0, 1, 0}}}
	if err := s.PutChunks(ctx, added); err != nil {
		t.Fatalf("put2: %v", err)
	}
	res, _, err := s.SearchChunks(ctx, "proj", []float32{0, 1, 0}, "", 1, false)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res) != 1 || res[0].ID != "c2" {
		t.Fatalf("cache did not invalidate: %+v", res)
	}
}

// TestVectorIndexReopenRebuildsFromSQL: opening the same on-disk database
// from a fresh Store instance must rebuild the index from the persisted
// chunks. Guards against a cache-only design that would silently answer
// nothing on the second process.
func TestVectorIndexReopenRebuildsFromSQL(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dsn := filepath.Join(dir, "reopen.db")

	s1, err := Open(ctx, "sqlite", dsn, 4, 5000, true)
	if err != nil {
		t.Fatalf("open1: %v", err)
	}
	if err := s1.PutChunks(ctx, []Chunk{
		{ID: "c1", Collection: "proj", Content: "one", Embedding: []float32{1, 0}},
		{ID: "c2", Collection: "proj", Content: "two", Embedding: []float32{0, 1}},
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("close1: %v", err)
	}

	s2, err := Open(ctx, "sqlite", dsn, 4, 5000, true)
	if err != nil {
		t.Fatalf("open2: %v", err)
	}
	t.Cleanup(func() { s2.Close() })
	res, _, err := s2.SearchChunks(ctx, "proj", []float32{0, 1}, "", 1, false)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res) != 1 || res[0].ID != "c2" {
		t.Fatalf("reopen lost data: %+v", res)
	}
}

// TestVectorIndexHybridLexical: a query whose dense pass would miss the
// right chunk (orthogonal vector) still hits it through the lexical side of
// the RRF blend.
func TestVectorIndexHybridLexical(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// c1 is the dense winner for [1,0,0]; c2 is the lex winner for "kubernetes".
	if err := s.PutChunks(ctx, []Chunk{
		{ID: "c1", Collection: "proj", Content: "unrelated content", Embedding: []float32{1, 0, 0}},
		{ID: "c2", Collection: "proj", Content: "kubernetes scheduler internals", Embedding: []float32{0, 1, 0}},
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	res, _, err := s.SearchChunks(ctx, "proj", []float32{1, 0, 0}, "kubernetes", 2, true)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	ids := map[string]bool{}
	for _, r := range res {
		ids[r.ID] = true
	}
	if !ids["c2"] {
		t.Fatalf("hybrid missed lexical match: %+v", res)
	}
}

// TestVectorIndexDimensionError: a batch whose vectors disagree with the
// collection's established dimensionality must be rejected, not panic hnsw.
func TestVectorIndexDimensionError(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.PutChunks(ctx, []Chunk{
		{ID: "c1", Collection: "proj", Content: "seed", Embedding: []float32{1, 0, 0}},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	err := s.PutChunks(ctx, []Chunk{
		{ID: "c2", Collection: "proj", Content: "bad", Embedding: []float32{1, 0}},
	})
	if err == nil {
		t.Fatalf("expected dimension mismatch error, got nil")
	}
	var dimErr *dimensionMismatchError
	if !errors.As(err, &dimErr) {
		t.Fatalf("want *dimensionMismatchError, got %T: %v", err, err)
	}
}

// TestVectorIndexRejectsBadEmbedding rejects empty, all-zero, and non-finite
// vectors before they can reach the graph.
func TestVectorIndexRejectsBadEmbedding(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	cases := []struct {
		name string
		vec  []float32
	}{
		{"empty", nil},
		{"zero", []float32{0, 0, 0}},
		{"nan", []float32{float32(math.NaN()), 0, 0}},
		{"inf", []float32{float32(math.Inf(1)), 0, 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := s.PutChunks(ctx, []Chunk{{ID: "bad", Collection: "col", Content: "x", Embedding: tc.vec}})
			if err == nil || !errors.Is(err, ErrEmbeddingShape) {
				t.Fatalf("want ErrEmbeddingShape, got %v", err)
			}
		})
	}
}

// TestVectorIndexReplaceDocumentsPrunesTail ensures a shrunken re-embed
// removes the higher-index chunks the previous, longer version left behind.
func TestVectorIndexReplaceDocumentsPrunesTail(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t).(*sqlStore)

	// Old version of doc had 3 chunks.
	if err := s.PutChunks(ctx, []Chunk{
		{ID: "d1_0", Collection: "col", DocID: "d1", Index: 0, Content: "old0", Embedding: []float32{1, 0}},
		{ID: "d1_1", Collection: "col", DocID: "d1", Index: 1, Content: "old1", Embedding: []float32{0, 1}},
		{ID: "d1_2", Collection: "col", DocID: "d1", Index: 2, Content: "old2", Embedding: []float32{1, 1}},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// New version has only 2 chunks — index 2 must be pruned.
	if err := s.ReplaceDocuments(ctx, "col", []Chunk{
		{ID: "d1_0", Collection: "col", DocID: "d1", Index: 0, Content: "new0", Embedding: []float32{1, 0}},
		{ID: "d1_1", Collection: "col", DocID: "d1", Index: 1, Content: "new1", Embedding: []float32{0, 1}},
	}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	row := s.row(ctx, `SELECT COUNT(*) FROM rag_chunks WHERE collection=? AND doc_id=?`, "col", "d1")
	var n int
	if err := row.Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("stale tail not pruned: have %d rows", n)
	}
	// The dropped chunk must not show up in a search that would have
	// matched it before.
	res, _, err := s.SearchChunks(ctx, "col", []float32{1, 1}, "", 3, false)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	for _, r := range res {
		if r.ID == "d1_2" {
			t.Fatalf("pruned chunk still searchable: %+v", r)
		}
	}
}

// TestVectorIndexDeleteCollectionClearsCache: DeleteCollection must drop the
// cache entry so a subsequent search on the same name (after new writes)
// does not answer from the stale graph.
func TestVectorIndexDeleteCollectionClearsCache(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t).(*sqlStore)

	if err := s.PutChunks(ctx, []Chunk{
		{ID: "c1", Collection: "col", Content: "a", Embedding: []float32{1, 0}},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, _, err := s.SearchChunks(ctx, "col", []float32{1, 0}, "", 1, false); err != nil {
		t.Fatalf("warm: %v", err)
	}
	if _, err := s.DeleteCollection(ctx, "col"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	s.ragMu.Lock()
	_, present := s.ragCache["col"]
	s.ragMu.Unlock()
	if present {
		t.Fatalf("cache entry survived DeleteCollection")
	}
	res, _, err := s.SearchChunks(ctx, "col", []float32{1, 0}, "", 1, false)
	if err != nil {
		t.Fatalf("search after delete: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("collection still returns rows: %+v", res)
	}
}

// TestVectorIndexPostgres runs the same nearest-neighbor + hybrid contract
// against a real Postgres schema created by openPostgresForTest. Skipped
// when TEST_POSTGRES_DSN is unset.
func TestVectorIndexPostgres(t *testing.T) {
	dsn := openPostgresForTest(t)
	ctx := context.Background()
	s, err := Open(ctx, "postgres", dsn, 4, 5000, false)
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	col := "test_vector_index"
	if err := s.PutChunks(ctx, []Chunk{
		{ID: "c1", Collection: col, Content: "postgres tuning notes", Embedding: []float32{1, 0, 0}},
		{ID: "c2", Collection: col, Content: "sqlite in the harness", Embedding: []float32{0, 1, 0}},
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	res, _, err := s.SearchChunks(ctx, col, []float32{1, 0, 0}, "postgres", 2, true)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res) == 0 || res[0].ID != "c1" {
		t.Fatalf("postgres hybrid did not rank c1 first: %+v", res)
	}
}

// TestVectorIndexRevisionMonotonicAcrossRecreate ensures DeleteCollection
// does NOT rewind the collection's revision counter — a cross-process cache
// tracking that counter would otherwise be fooled by a delete+re-insert of
// the same shape into serving the previous graph. The counter is the
// staleness signal; monotonicity is its correctness precondition.
func TestVectorIndexRevisionMonotonicAcrossRecreate(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t).(*sqlStore)

	if err := s.PutChunks(ctx, []Chunk{
		{ID: "c1", Collection: "col", Content: "one", Embedding: []float32{1, 0}},
	}); err != nil {
		t.Fatalf("put1: %v", err)
	}
	var rev1 int64
	if err := s.row(ctx, `SELECT revision FROM rag_collection_revisions WHERE collection=?`, "col").Scan(&rev1); err != nil {
		t.Fatalf("rev1: %v", err)
	}
	if _, err := s.DeleteCollection(ctx, "col"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// Revision row must survive the delete.
	var rev2 int64
	if err := s.row(ctx, `SELECT revision FROM rag_collection_revisions WHERE collection=?`, "col").Scan(&rev2); err != nil {
		t.Fatalf("rev after delete: %v (row must persist for monotonic counter)", err)
	}
	if rev2 < rev1 {
		t.Fatalf("revision went backwards across delete: %d -> %d", rev1, rev2)
	}
	// Re-insert same shape — revision must advance further, not reset.
	if err := s.PutChunks(ctx, []Chunk{
		{ID: "c1", Collection: "col", Content: "two", Embedding: []float32{1, 0}},
	}); err != nil {
		t.Fatalf("put2: %v", err)
	}
	var rev3 int64
	if err := s.row(ctx, `SELECT revision FROM rag_collection_revisions WHERE collection=?`, "col").Scan(&rev3); err != nil {
		t.Fatalf("rev3: %v", err)
	}
	if rev3 <= rev2 {
		t.Fatalf("revision did not advance across recreate: %d -> %d", rev2, rev3)
	}
}

// TestVectorIndexRejectsCrossCollectionID: a chunk id already stored in
// collection A cannot be re-upserted under collection B; that would silently
// move the row out from under A's index and answer A's queries with a hole.
func TestVectorIndexRejectsCrossCollectionID(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.PutChunks(ctx, []Chunk{
		{ID: "shared", Collection: "A", Content: "in A", Embedding: []float32{1, 0}},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	err := s.PutChunks(ctx, []Chunk{
		{ID: "shared", Collection: "B", Content: "in B", Embedding: []float32{0, 1}},
	})
	if err == nil {
		t.Fatalf("expected cross-collection rejection, got nil")
	}
	if !strings.Contains(err.Error(), "already stored in collection") {
		t.Fatalf("unexpected error text: %v", err)
	}
}

// TestVectorIndexLegacyBadVectorSurfacesError: a poisoned rag_chunks row
// (all-zero embedding written by an older binary) must produce a
// descriptive ErrLegacyBadVector on the next search rebuild, not a silent
// hole in the index.
func TestVectorIndexLegacyBadVectorSurfacesError(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t).(*sqlStore)

	// Insert directly, bypassing PutChunks' validation, to simulate a
	// legacy row.
	if _, err := s.exec(ctx,
		`INSERT INTO rag_chunks (id,collection,doc_id,path,chunk_index,content,embedding,meta,created_at)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		"bad", "col", "", "", 0, "junk", encodeEmbedding([]float32{0, 0, 0}), "{}", ms(time.Now())); err != nil {
		t.Fatalf("raw insert: %v", err)
	}
	_, _, err := s.SearchChunks(ctx, "col", []float32{1, 0, 0}, "", 1, false)
	if err == nil {
		t.Fatalf("expected ErrLegacyBadVector, got nil")
	}
	if !errors.Is(err, ErrLegacyBadVector) {
		t.Fatalf("want ErrLegacyBadVector, got %v", err)
	}
}

// TestVectorIndexDeleteDocumentsDropsEmpty: the builtin RAG provider calls
// DeleteDocuments when a re-embedded doc chunked to nothing; that path must
// remove the previous version's chunks so an empty re-index does not
// silently retain stale content.
func TestVectorIndexDeleteDocumentsDropsEmpty(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t).(*sqlStore)

	if err := s.PutChunks(ctx, []Chunk{
		{ID: "d_0", Collection: "col", DocID: "d", Index: 0, Content: "one", Embedding: []float32{1, 0}},
		{ID: "d_1", Collection: "col", DocID: "d", Index: 1, Content: "two", Embedding: []float32{0, 1}},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	n, err := s.DeleteDocuments(ctx, "col", []string{"d"})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n != 2 {
		t.Fatalf("want 2 rows removed, got %d", n)
	}
	res, _, err := s.SearchChunks(ctx, "col", []float32{1, 0}, "", 3, false)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("doc chunks still present: %+v", res)
	}
}

// buildBenchStore seeds `n` chunks of `dim` unit vectors into a fresh
// SQLite store and returns the store, the sourced ids, the sourced vecs, a
// warmed cache, and a deterministic query vector. Shared by every
// vector-index benchmark so the corpus + query are identical across
// baselines.
func buildBenchStore(b *testing.B, n, dim int) (Store, []string, [][]float32, []float32) {
	b.Helper()
	ctx := context.Background()
	dsn := filepath.Join(b.TempDir(), "bench.db")
	s, err := Open(ctx, "sqlite", dsn, 4, 5000, true)
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	b.Cleanup(func() { s.Close() })

	rng := rand.New(rand.NewPCG(1, 2))
	vecs := make([][]float32, n)
	ids := make([]string, n)
	chunks := make([]Chunk, n)
	for i := range n {
		vecs[i] = unitVector(rng, dim)
		ids[i] = fmt.Sprintf("c%05d", i)
		chunks[i] = Chunk{ID: ids[i], Collection: "bench", Content: ids[i], Embedding: vecs[i]}
	}
	if err := s.PutChunks(ctx, chunks); err != nil {
		b.Fatalf("put: %v", err)
	}
	q := unitVector(rng, dim)
	// Warm the HNSW cache so the first benchmark iteration is not
	// distorted by the one-time build cost.
	if _, _, err := s.SearchChunks(ctx, "bench", q, "", 10, false); err != nil {
		b.Fatalf("warm: %v", err)
	}
	return s, ids, vecs, q
}

// bruteForceTopK returns the k ids with the largest cosine similarity to q,
// computed with the same Go cosine used across the store. Truth source for
// recall metrics.
func bruteForceTopK(ids []string, vecs [][]float32, q []float32, k int) map[string]bool {
	type pair struct {
		id  string
		sim float64
	}
	all := make([]pair, len(ids))
	for i := range ids {
		all[i] = pair{id: ids[i], sim: cosine(q, vecs[i])}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].sim > all[j].sim })
	out := make(map[string]bool, k)
	for i := range k {
		out[all[i].id] = true
	}
	return out
}

// benchmarkWarmHNSW is the representative production hot path: SearchChunks
// against a warmed per-collection HNSW cache. Recall is measured against
// bruteForceTopK using the same corpus.
func benchmarkWarmHNSW(b *testing.B, n, dim int) {
	ctx := context.Background()
	s, ids, vecs, q := buildBenchStore(b, n, dim)
	truth := bruteForceTopK(ids, vecs, q, 10)

	got, _, err := s.SearchChunks(ctx, "bench", q, "", 10, false)
	if err != nil {
		b.Fatalf("search: %v", err)
	}
	overlap := 0
	for _, r := range got {
		if truth[r.ID] {
			overlap++
		}
	}
	recall := float64(overlap) / 10.0
	if recall < 0.7 {
		b.Fatalf("recall@10=%.2f below floor 0.70", recall)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, _, err := s.SearchChunks(ctx, "bench", q, "", 10, false); err != nil {
			b.Fatalf("search: %v", err)
		}
	}
	b.ReportMetric(recall, "recall@10")
}

// benchmarkSQLScanBaseline reproduces the pre-HNSW code path against the
// SAME on-disk SQLite store: SELECT every (id, content, embedding) row for
// the collection, decode each embedding, compute cosine in Go, sort, take
// top-k. This is the fair baseline for measuring the HNSW cache's win — it
// includes the SQL scan, blob decode, and per-row float math the old
// SearchChunks used to pay on every query.
func benchmarkSQLScanBaseline(b *testing.B, n, dim int) {
	ctx := context.Background()
	s, _, _, q := buildBenchStore(b, n, dim)
	ss := s.(*sqlStore)

	scan := func() {
		rows, err := ss.query(ctx, `SELECT id, content, embedding FROM rag_chunks WHERE collection=?`, "bench")
		if err != nil {
			b.Fatalf("select: %v", err)
		}
		type row struct {
			id  string
			sim float64
		}
		out := make([]row, 0, n)
		for rows.Next() {
			var (
				id      string
				content string
				raw     []byte
			)
			if err := rows.Scan(&id, &content, &raw); err != nil {
				rows.Close()
				b.Fatalf("scan: %v", err)
			}
			v := decodeEmbedding(raw)
			out = append(out, row{id: id, sim: cosine(q, v)})
		}
		rows.Close()
		sort.Slice(out, func(i, j int) bool { return out[i].sim > out[j].sim })
		_ = out[:10]
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		scan()
	}
}

// benchmarkPureCosine reports the physical floor: no SQL, no HNSW, just
// cosine over the in-memory corpus. Useful only as a lower bound; the
// production SQL baseline (benchmarkSQLScanBaseline) is the number to beat.
func benchmarkPureCosine(b *testing.B, n, dim int) {
	rng := rand.New(rand.NewPCG(1, 2))
	vecs := make([][]float32, n)
	for i := range n {
		vecs[i] = unitVector(rng, dim)
	}
	q := unitVector(rng, dim)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		var best float64 = -2
		var bestIdx int
		for j := range n {
			c := cosine(q, vecs[j])
			if c > best {
				best = c
				bestIdx = j
			}
		}
		_ = bestIdx
	}
}

// 32-dim: intentionally adversarial for HNSW (tiny per-distance cost, graph
// traversal overhead dominates). Kept to document the crossover.
func BenchmarkVectorIndex_WarmHNSW_5000x32(b *testing.B)   { benchmarkWarmHNSW(b, 5000, 32) }
func BenchmarkVectorIndex_SQLScan_5000x32(b *testing.B)    { benchmarkSQLScanBaseline(b, 5000, 32) }
func BenchmarkVectorIndex_PureCosine_5000x32(b *testing.B) { benchmarkPureCosine(b, 5000, 32) }

// 768-dim: representative production shape (OpenAI text-embedding-3-small,
// most gateway defaults). This is the number that matters for the "no
// per-query full collection scan" acceptance bar.
func BenchmarkVectorIndex_WarmHNSW_5000x768(b *testing.B)   { benchmarkWarmHNSW(b, 5000, 768) }
func BenchmarkVectorIndex_SQLScan_5000x768(b *testing.B)    { benchmarkSQLScanBaseline(b, 5000, 768) }
func BenchmarkVectorIndex_PureCosine_5000x768(b *testing.B) { benchmarkPureCosine(b, 5000, 768) }
