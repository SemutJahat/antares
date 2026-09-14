package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

const memoryCols = `id,scope,scope_key,mem_key,content,tags,source,pinned,created_at,updated_at`

func scanMemory(sc interface{ Scan(...any) error }) (*Memory, error) {
	var m Memory
	var created, upd int64
	if err := sc.Scan(&m.ID, &m.Scope, &m.ScopeKey, &m.Key, &m.Content, &m.Tags, &m.Source, &m.Pinned, &created, &upd); err != nil {
		return nil, err
	}
	m.CreatedAt, m.UpdatedAt = fromMS(created), fromMS(upd)
	return &m, nil
}

func (s *sqlStore) PutMemory(ctx context.Context, m *Memory) error {
	now := time.Now()
	if m.CreatedAt.IsZero() {
		m.CreatedAt = now
	}
	m.UpdatedAt = now
	if m.Tags == "" {
		m.Tags = "[]"
	}
	if m.Scope == "" {
		m.Scope = "global"
	}
	_, err := s.exec(ctx, `INSERT INTO memories (`+memoryCols+`) VALUES (?,?,?,?,?,?,?,?,?,?)`+
		onConflict("id", "content=EXCLUDED.content, mem_key=EXCLUDED.mem_key, tags=EXCLUDED.tags, pinned=EXCLUDED.pinned, updated_at=EXCLUDED.updated_at"),
		m.ID, m.Scope, m.ScopeKey, m.Key, m.Content, m.Tags, m.Source, m.Pinned, ms(m.CreatedAt), ms(m.UpdatedAt))
	return err
}

func (s *sqlStore) ListMemories(ctx context.Context, scope, scopeKey string, limit int) ([]Memory, error) {
	var where []string
	var args []any
	if scope != "" && scope != "all" {
		where = append(where, "scope=?")
		args = append(args, scope)
	}
	if scopeKey != "" {
		where = append(where, "scope_key=?")
		args = append(args, scopeKey)
	}
	clause := ""
	if len(where) > 0 {
		clause = " WHERE " + strings.Join(where, " AND ")
	}
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.query(ctx, `SELECT `+memoryCols+` FROM memories`+clause+
		` ORDER BY pinned DESC, updated_at DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Memory, 0, limit)
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

func (s *sqlStore) SearchMemories(ctx context.Context, query string, limit int) ([]Memory, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return s.ListMemories(ctx, "", "", limit)
	}
	if limit <= 0 || limit > 200 {
		limit = 30
	}
	var (
		rows *sql.Rows
		err  error
	)
	if s.dialect == "sqlite" {
		rows, err = s.query(ctx, `SELECT `+prefixCols("m", memoryCols)+` FROM memories_fts f
			JOIN memories m ON m.id = f.memory_id WHERE memories_fts MATCH ?
			ORDER BY bm25(memories_fts) LIMIT ?`, ftsQuery(query), limit)
	} else {
		rows, err = s.query(ctx, `SELECT `+memoryCols+` FROM memories
			WHERE to_tsvector('simple', content) @@ plainto_tsquery('simple', ?)
			ORDER BY ts_rank(to_tsvector('simple', content), plainto_tsquery('simple', ?)) DESC LIMIT ?`,
			query, query, limit)
	}
	if err != nil {
		rows, err = s.query(ctx, `SELECT `+memoryCols+` FROM memories WHERE LOWER(content) LIKE ?
			ORDER BY updated_at DESC LIMIT ?`, "%"+strings.ToLower(query)+"%", limit)
		if err != nil {
			return nil, err
		}
	}
	defer rows.Close()
	out := make([]Memory, 0, limit)
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

func prefixCols(alias, cols string) string {
	parts := strings.Split(cols, ",")
	for i, p := range parts {
		parts[i] = alias + "." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ",")
}

func (s *sqlStore) DeleteMemory(ctx context.Context, id string) error {
	_, err := s.exec(ctx, `DELETE FROM memories WHERE id=?`, id)
	return err
}

func (s *sqlStore) ClearMemories(ctx context.Context, scope, scopeKey string) (int64, error) {
	q := `DELETE FROM memories WHERE pinned = FALSE`
	var args []any
	if scope != "" && scope != "all" {
		q += ` AND scope=?`
		args = append(args, scope)
	}
	if scopeKey != "" {
		q += ` AND scope_key=?`
		args = append(args, scopeKey)
	}
	res, err := s.exec(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ---- RAG chunks -------------------------------------------------------------

// PutChunks upserts one batch of embedded chunks. Every chunk is validated
// (finite, non-zero, cross-checked against the collection's persisted
// dimensionality) before any row is written — hnsw.Graph.assertDims panics
// on a dimension mismatch, so a bad vector must be refused at the SQL
// boundary rather than at the graph.
//
// Cross-instance dimension enforcement lives in the rag_collection_revisions
// row: the write transaction locks that row FOR UPDATE (Postgres) or relies
// on the SQLite serialized writer, upserts (collection, dims) with a
// coalesce, then inserts chunks under the same tx. Two writers hitting the
// same empty collection with different dims cannot both succeed — one loses
// on the row lock and re-reads the stamped dims before writing.
//
// A batch that touches multiple collections is written under one tx; the
// dimension guard runs per-collection. A single chunk id whose payload
// disagrees with the persisted row's collection is rejected loudly — same
// id crossing collections would silently mutate a foreign collection's
// index because the upsert key is id-only.
func (s *sqlStore) PutChunks(ctx context.Context, chunks []Chunk) error {
	if len(chunks) == 0 {
		return nil
	}

	byCollection := make(map[string][]int, 1)
	for i, c := range chunks {
		if c.Collection == "" {
			return fmt.Errorf("store: chunk %q missing collection", c.ID)
		}
		if c.ID == "" {
			return fmt.Errorf("store: chunk in collection %q missing id", c.Collection)
		}
		if err := validateEmbedding(c.Embedding); err != nil {
			return fmt.Errorf("store: chunk %q: %w", c.ID, err)
		}
		byCollection[c.Collection] = append(byCollection[c.Collection], i)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Enforce collection dimensionality under the transaction so two
	// writers on separate instances cannot both stamp fresh dims.
	for col, idxs := range byCollection {
		want := len(chunks[idxs[0]].Embedding)
		for _, i := range idxs {
			if len(chunks[i].Embedding) != want {
				return &dimensionMismatchError{want: want, got: len(chunks[i].Embedding)}
			}
		}
		got, err := s.reserveCollectionDims(ctx, tx, col, want)
		if err != nil {
			return err
		}
		if got != want {
			return &dimensionMismatchError{want: got, got: want}
		}
	}

	// Reject a chunk whose id already lives in a different collection —
	// PutChunks would otherwise silently rewrite the foreign row because
	// the id is the sole upsert key.
	if err := s.rejectCrossCollectionIDs(ctx, tx, chunks); err != nil {
		return err
	}

	stmt, err := tx.PrepareContext(ctx, s.rebind(
		`INSERT INTO rag_chunks (id,collection,doc_id,path,chunk_index,content,embedding,meta,created_at)
		 VALUES (?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
		   doc_id=EXCLUDED.doc_id,
		   path=EXCLUDED.path,
		   chunk_index=EXCLUDED.chunk_index,
		   content=EXCLUDED.content,
		   embedding=EXCLUDED.embedding,
		   meta=EXCLUDED.meta
		 WHERE rag_chunks.collection = EXCLUDED.collection`))
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, c := range chunks {
		if c.CreatedAt.IsZero() {
			c.CreatedAt = time.Now()
		}
		meta, _ := c.Meta.Value()
		res, err := stmt.ExecContext(ctx, c.ID, c.Collection, c.DocID, c.Path, c.Index, c.Content,
			encodeEmbedding(c.Embedding), meta, ms(c.CreatedAt))
		if err != nil {
			return err
		}
		// A concurrent writer could win the id insert into a different
		// collection between our precheck and here; the ON CONFLICT WHERE
		// then filters our DO UPDATE and leaves 0 rows affected. Refuse
		// loudly instead of silently no-op'ing the write.
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			return fmt.Errorf("store: chunk %q race lost to a concurrent writer under a different collection; retry", c.ID)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for col := range byCollection {
		s.invalidateCollection(col)
	}
	return nil
}

// reserveCollectionDims upserts the (collection, dims) row inside tx and
// returns the effective dims after the upsert. First writer stamps `want`;
// subsequent writers see the stamped value unchanged. Callers compare and
// reject a mismatch. Uses the same rag_collection_revisions row that the
// revision counter lives on so both are consistent by construction.
func (s *sqlStore) reserveCollectionDims(ctx context.Context, tx *sql.Tx, collection string, want int) (int, error) {
	// Postgres benefits from a row lock so two concurrent writers on
	// separate connections cannot both succeed with different dims.
	// SQLite serializes writers at the file level so no explicit lock is
	// needed there.
	if s.dialect == "postgres" {
		if _, err := tx.ExecContext(ctx, s.rebind(
			`INSERT INTO rag_collection_revisions(collection, revision, dims)
			 VALUES (?, 0, ?)
			 ON CONFLICT(collection) DO UPDATE SET dims = COALESCE(NULLIF(rag_collection_revisions.dims, 0), EXCLUDED.dims)`),
			collection, want); err != nil {
			return 0, err
		}
		var got int
		if err := tx.QueryRowContext(ctx, s.rebind(
			`SELECT dims FROM rag_collection_revisions WHERE collection=? FOR UPDATE`),
			collection).Scan(&got); err != nil {
			return 0, err
		}
		return got, nil
	}
	// SQLite: same upsert without FOR UPDATE.
	if _, err := tx.ExecContext(ctx, s.rebind(
		`INSERT INTO rag_collection_revisions(collection, revision, dims)
		 VALUES (?, 0, ?)
		 ON CONFLICT(collection) DO UPDATE SET dims = CASE WHEN dims = 0 THEN excluded.dims ELSE dims END`),
		collection, want); err != nil {
		return 0, err
	}
	var got int
	if err := tx.QueryRowContext(ctx, s.rebind(
		`SELECT dims FROM rag_collection_revisions WHERE collection=?`),
		collection).Scan(&got); err != nil {
		return 0, err
	}
	return got, nil
}

// rejectCrossCollectionIDs errors if any chunk id is already stored under a
// different collection than the batch declares. Otherwise the id-keyed
// upsert would silently mutate the foreign collection's chunk out from
// under it.
func (s *sqlStore) rejectCrossCollectionIDs(ctx context.Context, tx *sql.Tx, chunks []Chunk) error {
	want := make(map[string]string, len(chunks))
	ids := make([]string, 0, len(chunks))
	for _, c := range chunks {
		if _, seen := want[c.ID]; !seen {
			ids = append(ids, c.ID)
		}
		want[c.ID] = c.Collection
	}
	// Batch-friendly IN(?,?,…) probe.
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	rows, err := tx.QueryContext(ctx, s.rebind(
		`SELECT id, collection FROM rag_chunks WHERE id IN (`+strings.Join(placeholders, ",")+`)`),
		args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, existing string
		if err := rows.Scan(&id, &existing); err != nil {
			return err
		}
		if want[id] != existing {
			return fmt.Errorf("store: chunk %q already stored in collection %q; refusing to move it to %q",
				id, existing, want[id])
		}
	}
	return rows.Err()
}

// SearchChunks returns the top-k nearest chunks for embedding, blending in a
// lexical FTS pass via reciprocal-rank fusion when hybrid is set. The dense
// pass runs against a per-collection HNSW cache so no query scans the whole
// table; the cache invalidates on revision bumps from the rag_chunks
// triggers so an out-of-process writer stays visible.
func (s *sqlStore) SearchChunks(ctx context.Context, collection string, embedding []float32, text string, topK int, hybrid bool) ([]Chunk, []float64, error) {
	if topK <= 0 {
		topK = 8
	}
	recall := topK

	dense, err := s.searchDense(ctx, collection, embedding, recall)
	if err != nil {
		return nil, nil, err
	}

	var lex []lexHit
	if hybrid {
		lex, err = s.searchLexical(ctx, collection, text, recall*2)
		if err != nil {
			return nil, nil, err
		}
	}

	if !hybrid {
		ids := make([]string, len(dense))
		for i, d := range dense {
			ids[i] = d.id
		}
		chunks, err := s.hydrateChunks(ctx, collection, ids)
		if err != nil {
			return nil, nil, err
		}
		outC := make([]Chunk, 0, len(dense))
		outS := make([]float64, 0, len(dense))
		for _, d := range dense {
			c, ok := chunks[d.id]
			if !ok {
				continue
			}
			outC = append(outC, c)
			outS = append(outS, d.sim)
		}
		return outC, outS, nil
	}

	// Reciprocal-rank fusion. k=60 is the standard tuning constant.
	const rrfK = 60.0
	fused := make(map[string]float64, len(dense)+len(lex))
	for rank, d := range dense {
		fused[d.id] += 1.0 / (rrfK + float64(rank))
	}
	for rank, l := range lex {
		fused[l.id] += 1.0 / (rrfK + float64(rank))
	}
	if len(fused) == 0 {
		return nil, nil, nil
	}
	type ranked struct {
		id    string
		score float64
	}
	all := make([]ranked, 0, len(fused))
	for id, sc := range fused {
		all = append(all, ranked{id: id, score: sc})
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].score != all[j].score {
			return all[i].score > all[j].score
		}
		return all[i].id < all[j].id
	})
	if len(all) > topK {
		all = all[:topK]
	}
	ids := make([]string, len(all))
	for i, r := range all {
		ids[i] = r.id
	}
	chunks, err := s.hydrateChunks(ctx, collection, ids)
	if err != nil {
		return nil, nil, err
	}
	outC := make([]Chunk, 0, len(all))
	outS := make([]float64, 0, len(all))
	for _, r := range all {
		c, ok := chunks[r.id]
		if !ok {
			continue
		}
		outC = append(outC, c)
		outS = append(outS, r.score)
	}
	return outC, outS, nil
}

// DeleteCollection removes every chunk for collection. The
// rag_collection_revisions row is retained so its `revision` counter stays
// monotonic across the whole database lifetime — a cross-process cache that
// tracks the counter cannot be fooled by a delete + re-insert (ABA) into
// answering from the previous graph. The `dims` field IS reset to 0 so a
// deliberate re-embed with a new dimensionality after a full wipe is
// permitted; the trigger-driven revision bump on the next insert still
// advances the counter, so cache invalidation is preserved.
func (s *sqlStore) DeleteCollection(ctx context.Context, collection string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	// Lock the same collection row writers reserve before changing chunks.
	if _, err := tx.ExecContext(ctx, s.rebind(`UPDATE rag_collection_revisions SET dims=0 WHERE collection=?`), collection); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, s.rebind(`DELETE FROM rag_chunks WHERE collection=?`), collection)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	s.invalidateCollection(collection)
	return n, nil
}

// ReplaceDocuments upserts a batch of chunks and, for each represented docID,
// deletes any chunks whose chunk_index is strictly greater than the largest
// index kept in the batch for that document. This is the primitive the
// builtin RAG provider uses to shrink a re-embedded document without leaving
// stale tail chunks behind — a plain PutChunks would keep those rows and
// pollute future searches.
//
// The primary write and the tail cleanup happen in one transaction so a
// crash between them cannot leave a document half-updated. Dimension
// enforcement uses the same reserveCollectionDims path as PutChunks.
func (s *sqlStore) ReplaceDocuments(ctx context.Context, collection string, chunks []Chunk) error {
	if collection == "" {
		return fmt.Errorf("store: ReplaceDocuments requires a collection")
	}
	if len(chunks) == 0 {
		return nil
	}
	keepMax := make(map[string]int, 4)
	for _, c := range chunks {
		if c.Collection != collection {
			return fmt.Errorf("store: ReplaceDocuments chunk %q in wrong collection %q (batch %q)",
				c.ID, c.Collection, collection)
		}
		if c.ID == "" {
			return fmt.Errorf("store: ReplaceDocuments chunk missing id (doc %q)", c.DocID)
		}
		if c.DocID == "" {
			return fmt.Errorf("store: ReplaceDocuments chunk %q missing doc_id", c.ID)
		}
		if err := validateEmbedding(c.Embedding); err != nil {
			return fmt.Errorf("store: chunk %q: %w", c.ID, err)
		}
		if v, ok := keepMax[c.DocID]; !ok || c.Index > v {
			keepMax[c.DocID] = c.Index
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	want := len(chunks[0].Embedding)
	for _, c := range chunks {
		if len(c.Embedding) != want {
			return &dimensionMismatchError{want: want, got: len(c.Embedding)}
		}
	}
	got, err := s.reserveCollectionDims(ctx, tx, collection, want)
	if err != nil {
		return err
	}
	if got != want {
		return &dimensionMismatchError{want: got, got: want}
	}

	if err := s.rejectCrossCollectionIDs(ctx, tx, chunks); err != nil {
		return err
	}

	stmt, err := tx.PrepareContext(ctx, s.rebind(
		`INSERT INTO rag_chunks (id,collection,doc_id,path,chunk_index,content,embedding,meta,created_at)
		 VALUES (?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
		   doc_id=EXCLUDED.doc_id,
		   path=EXCLUDED.path,
		   chunk_index=EXCLUDED.chunk_index,
		   content=EXCLUDED.content,
		   embedding=EXCLUDED.embedding,
		   meta=EXCLUDED.meta
		 WHERE rag_chunks.collection = EXCLUDED.collection`))
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, c := range chunks {
		if c.CreatedAt.IsZero() {
			c.CreatedAt = time.Now()
		}
		meta, _ := c.Meta.Value()
		res, err := stmt.ExecContext(ctx, c.ID, c.Collection, c.DocID, c.Path, c.Index, c.Content,
			encodeEmbedding(c.Embedding), meta, ms(c.CreatedAt))
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			return fmt.Errorf("store: chunk %q race lost to a concurrent writer under a different collection; retry", c.ID)
		}
	}
	delStmt, err := tx.PrepareContext(ctx, s.rebind(
		`DELETE FROM rag_chunks WHERE collection=? AND doc_id=? AND chunk_index>?`))
	if err != nil {
		return err
	}
	defer delStmt.Close()
	for docID, maxIdx := range keepMax {
		if _, err := delStmt.ExecContext(ctx, collection, docID, maxIdx); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.invalidateCollection(collection)
	return nil
}

// DeleteDocuments removes every chunk belonging to docIDs inside collection.
// This is the primitive that lets the builtin RAG provider drop a document
// entirely when its re-index yielded zero chunks — without it an empty
// re-embed silently retained the previous version. Missing docIDs are
// simply skipped; the returned count is the number of chunks actually
// deleted.
func (s *sqlStore) DeleteDocuments(ctx context.Context, collection string, docIDs []string) (int64, error) {
	if collection == "" {
		return 0, fmt.Errorf("store: DeleteDocuments requires a collection")
	}
	if len(docIDs) == 0 {
		return 0, nil
	}
	placeholders := make([]string, len(docIDs))
	args := make([]any, 0, len(docIDs)+1)
	args = append(args, collection)
	for i, id := range docIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	q := `DELETE FROM rag_chunks WHERE collection=? AND doc_id IN (` +
		strings.Join(placeholders, ",") + `)`
	res, err := s.exec(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	s.invalidateCollection(collection)
	return n, nil
}

func (s *sqlStore) ListCollections(ctx context.Context) ([]string, error) {
	rows, err := s.query(ctx, `SELECT DISTINCT collection FROM rag_chunks ORDER BY collection`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
