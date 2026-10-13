package repository

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

var _ RepoIndex = &sqliteRepoIndex{}

// sqliteRepoIndex keeps the indexes of every namespace in a single SQLite
// database. SQLite does the locking between processes: in WAL mode, readers
// see the latest committed state while a single writer works.
type sqliteRepoIndex struct {
	path string

	mu sync.Mutex
	db *sql.DB
}

// NewSqliteIndex returns the indexes in the SQLite database at path, created as
// needed, or in memory if path is ":memory:".
func NewSqliteIndex(path string) RepoIndex {
	return &sqliteRepoIndex{path: path}
}

// sqliteLayout is the version of the schema and of textTokens, kept in the
// user_version of the database. A database with another one has its tables
// recreated empty, and the cache fills them again.
const sqliteLayout = 1

// sqliteSchema holds the documents of every namespace:
//
//	namespaces            docs                              fts (FTS5)
//	+------------+        +-----------------------+         +--------+
//	| ns      PK |<-------| ns          FK        |         |        |
//	| version    | 1    * | num         PK        |-------->| rowid  |
//	+------------+        | id          per ns    | 1  0..1 | tokens |
//	                      | built_from  commit    |         +--------+
//	                      | excerpt     JSONB     |
//	                      +-----------------------+
//
// In detail:
//   - namespaces: one row per namespace ("bugs", "identities"), with the version
//     of the format of its documents, see GetIndex.
//   - docs: one row per document: the id of its entity, the commit it was built
//     from, and its excerpt, a JSON object in JSONB. num is only internal,
//     linking to fts.
//   - fts: the tokens of the texts of a document, as textTokens gives them, with
//     textsSeparator between two texts. The table is contentless: the tokens
//     are indexed, not stored, and can't be read back. A document without text
//     has no row.
//
// For instance, a bug titled "Crash on login", with a comment "It happens every
// time":
//
//	namespaces  ("bugs", 6)
//	docs        (12, "bugs", "0a1b…", "3f2e…", {"Title": "Crash on login", …})
//	fts         (12, "crash login _ happen everi time")
//
// Deleting a namespace deletes its documents, by the foreign key, and deleting
// a document deletes its tokens, by the trigger: a virtual table can't have a
// foreign key. SQLite reuses the num of a deleted document, which would
// otherwise inherit the tokens left behind.
const sqliteSchema = `
CREATE TABLE namespaces (
	ns      TEXT PRIMARY KEY,
	version INTEGER NOT NULL
) STRICT;
CREATE TABLE docs (
	num        INTEGER PRIMARY KEY,
	ns         TEXT NOT NULL REFERENCES namespaces ON DELETE CASCADE,
	id         TEXT NOT NULL,
	built_from TEXT NOT NULL,
	excerpt    BLOB NOT NULL CHECK (json_valid(excerpt, 8) AND json_type(excerpt) = 'object'), -- JSONB
	UNIQUE (ns, id)
) STRICT;
CREATE VIRTUAL TABLE fts USING fts5(
	tokens, content='', contentless_delete=1, tokenize="unicode61 tokenchars '_'"
);
CREATE TRIGGER docs_delete AFTER DELETE ON docs BEGIN
	DELETE FROM fts WHERE rowid = old.num;
END;`

// textsSeparator separates the tokens of two texts of a document, for a phrase
// not to span them: textTokens never gives it, so no phrase has it.
const textsSeparator = "_"

func (s *sqliteRepoIndex) GetIndex(namespace string, version uint) (Index, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.db == nil {
		db, err := openSqlite(s.path)
		if err != nil {
			return nil, err
		}
		s.db = db
	}

	// the write lock is only taken for a namespace to make, or of another version
	var current uint
	err := s.db.QueryRow("SELECT version FROM namespaces WHERE ns = ?", namespace).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) || err == nil && current != version {
		err = func() error {
			tx, err := s.db.Begin()
			if err != nil {
				return err
			}
			defer tx.Rollback()
			// replaced along with its documents, unless another process did already
			_, err = tx.Exec("DELETE FROM namespaces WHERE ns = ? AND version != ?", namespace, version)
			if err != nil {
				return err
			}
			_, err = tx.Exec("INSERT OR IGNORE INTO namespaces (ns, version) VALUES (?, ?)", namespace, version)
			if err != nil {
				return err
			}
			return tx.Commit()
		}()
	}
	if err != nil {
		return nil, err
	}

	return &sqliteIndex{db: s.db, ns: namespace}, nil
}

// sqliteRecoveryTimeout is how long opening a broken database waits for another
// process recovering it. A recovery takes a few syscalls, so running out means
// the lock was most likely left behind by a crash.
var sqliteRecoveryTimeout = 10 * time.Second

// openSqlite opens the database at path. A file that is not a database, or a
// corrupt one, is deleted and made again empty: the index only holds what the
// cache can rebuild from the repository.
//
// The recovery holds <path>.lock, created exclusively, for two processes never
// to recover at once: the second would delete the database the first just made,
// under its connections. Under the lock, the database is opened again, for one
// already recovered to be kept. The WAL files are deleted before the database,
// for a process opening it meanwhile to find it broken, and wait on the lock,
// rather than pair an empty database with the WAL of the broken one.
func openSqlite(path string) (*sql.DB, error) {
	db, err := openSqliteOnce(path)
	if path == ":memory:" || !isBrokenSqlite(err) {
		return db, err
	}

	lockPath := path + ".lock"
	deadline := time.Now().Add(sqliteRecoveryTimeout)
	wait := time.Millisecond
	for {
		lockErr := createFileExclusive(lockPath, nil)
		if lockErr == nil {
			break
		}
		if !errors.Is(lockErr, fs.ErrExist) {
			return nil, errors.Join(err, lockErr)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%w: %s exists: another process is recovering the index, or crashed doing so; if no git-bug process is running, remove it", err, lockPath)
		}
		time.Sleep(wait)
		wait = min(2*wait, 50*time.Millisecond)
	}
	defer os.Remove(lockPath)

	db, err = openSqliteOnce(path)
	if !isBrokenSqlite(err) {
		return db, err
	}
	// on Windows, a file can't be deleted while open: another process can be
	// opening the broken database, until it finds it broken and waits on the lock
	deadline = time.Now().Add(sqliteRecoveryTimeout)
	wait = time.Millisecond
	for _, suffix := range []string{"-wal", "-shm", ""} {
		for {
			rmErr := os.Remove(path + suffix)
			if rmErr == nil || errors.Is(rmErr, os.ErrNotExist) {
				break
			}
			if time.Now().After(deadline) {
				return nil, errors.Join(err, rmErr)
			}
			time.Sleep(wait)
			wait = min(2*wait, 50*time.Millisecond)
		}
	}
	return openSqliteOnce(path)
}

// isBrokenSqlite tells if err is from a file that is not a database, or a
// corrupt one.
func isBrokenSqlite(err error) bool {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	switch sqliteErr.Code() & 0xff {
	case sqlite3.SQLITE_NOTADB, sqlite3.SQLITE_CORRUPT:
		return true
	}
	return false
}

func openSqliteOnce(path string) (*sql.DB, error) {
	if path != ":memory:" {
		err := os.MkdirAll(filepath.Dir(path), os.ModePerm)
		if err != nil {
			return nil, err
		}
	}

	// A write transaction takes the lock as it starts, for two writers to wait
	// on each other rather than fail. synchronous=NORMAL can lose the latest
	// transactions on a power loss, which a rebuild recovers.
	// The path is escaped for the URI, where ? starts the options, # a fragment,
	// and % an escape.
	escaped := strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(path)
	db, err := sql.Open("sqlite", "file:"+escaped+
		"?_txlock=immediate&_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"+
		"&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, err
	}
	if path == ":memory:" {
		// each connection would have a database of its own
		db.SetMaxOpenConns(1)
	}

	// in a transaction, for another process to never see the tables half made
	tx, err := db.Begin()
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	defer tx.Rollback()
	var layout int
	err = tx.QueryRow("PRAGMA user_version").Scan(&layout)
	if err == nil && layout != sqliteLayout {
		_, err = tx.Exec("DROP TABLE IF EXISTS fts; DROP TABLE IF EXISTS docs; DROP TABLE IF EXISTS namespaces;" + sqliteSchema +
			fmt.Sprintf("PRAGMA user_version = %d;", sqliteLayout))
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func (s *sqliteRepoIndex) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

var _ Index = &sqliteIndex{}

// sqliteIndex is the index of a namespace, see sqliteRepoIndex.
type sqliteIndex struct {
	db *sql.DB
	ns string
}

func (i *sqliteIndex) NewBatch() IndexBatch {
	return &sqliteBatch{index: i, changes: make(map[string]*sqliteDoc)}
}

func (i *sqliteIndex) BuiltFrom() (map[string]Hash, error) {
	rows, err := i.db.Query("SELECT id, built_from FROM docs WHERE ns = ?", i.ns)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	builtFrom := make(map[string]Hash)
	for rows.Next() {
		var id string
		var commit Hash
		err = rows.Scan(&id, &commit)
		if err != nil {
			return nil, err
		}
		builtFrom[id] = commit
	}
	return builtFrom, rows.Err()
}

func (i *sqliteIndex) Excerpt(id string) (Hash, []byte, error) {
	var commit Hash
	var excerpt []byte
	err := i.db.QueryRow("SELECT built_from, json(excerpt) FROM docs WHERE ns = ? AND id = ?", i.ns, id).
		Scan(&commit, &excerpt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, ErrNotFound
	}
	return commit, excerpt, err
}

func (i *sqliteIndex) Excerpts() (map[string]Hash, map[string][]byte, error) {
	rows, err := i.db.Query("SELECT id, built_from, json(excerpt) FROM docs WHERE ns = ?", i.ns)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	builtFrom := make(map[string]Hash)
	excerpts := make(map[string][]byte)
	for rows.Next() {
		var id string
		var commit Hash
		var excerpt []byte
		err = rows.Scan(&id, &commit, &excerpt)
		if err != nil {
			return nil, nil, err
		}
		builtFrom[id] = commit
		excerpts[id] = excerpt
	}
	err = rows.Err()
	if err != nil {
		return nil, nil, err
	}
	return builtFrom, excerpts, nil
}

func (i *sqliteIndex) Search(phrases []string) ([]string, error) {
	// Each phrase is a string for FTS5, quoted, so never a query syntax. Its
	// tokens can't hold a quote.
	var match []string
	for _, phrase := range phrases {
		tokens := textTokens(phrase)
		if len(tokens) > 0 {
			match = append(match, `"`+strings.Join(tokens, " ")+`"`)
		}
	}
	if len(match) == 0 {
		return nil, nil
	}

	rows, err := i.db.Query(`SELECT docs.id FROM fts JOIN docs ON docs.num = fts.rowid
		WHERE fts MATCH ? AND docs.ns = ?`, strings.Join(match, " OR "), i.ns)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		err = rows.Scan(&id)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (i *sqliteIndex) Clear() error {
	_, err := i.db.Exec("DELETE FROM docs WHERE ns = ?", i.ns)
	return err
}

type sqliteBatch struct {
	index *sqliteIndex
	// the latest change of each document: its new version, or nil if removed
	changes map[string]*sqliteDoc
}

type sqliteDoc struct {
	commit Hash
	// text, as jsonb() takes a blob for JSONB already
	excerpt string
	// the tokens of the texts, see textTokens and textsSeparator
	tokens string
}

func (b *sqliteBatch) Set(id string, commit Hash, excerpt []byte, texts []string) error {
	if !commit.IsValid() {
		return fmt.Errorf("invalid commit %q for document %s", commit, id)
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(excerpt, &object) != nil || object == nil {
		return fmt.Errorf("excerpt of document %s is not a JSON object", id)
	}

	// the texts are analyzed here, not in Apply which holds the lock
	var parts []string
	for _, text := range texts {
		tokens := textTokens(text)
		if len(tokens) > 0 {
			parts = append(parts, strings.Join(tokens, " "))
		}
	}
	b.changes[id] = &sqliteDoc{
		commit:  commit,
		excerpt: string(excerpt),
		tokens:  strings.Join(parts, " "+textsSeparator+" "),
	}
	return nil
}

func (b *sqliteBatch) Remove(id string) {
	b.changes[id] = nil
}

func (b *sqliteBatch) Apply() error {
	ns := b.index.ns
	tx, err := b.index.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for id, doc := range b.changes {
		if doc == nil {
			_, err = tx.Exec("DELETE FROM docs WHERE ns = ? AND id = ?", ns, id)
			if err != nil {
				return err
			}
			continue
		}

		// a new version keeps its num: its previous tokens go first
		_, err = tx.Exec("DELETE FROM fts WHERE rowid = (SELECT num FROM docs WHERE ns = ? AND id = ?)", ns, id)
		if err != nil {
			return err
		}

		var num int64
		err = tx.QueryRow(`INSERT INTO docs (ns, id, built_from, excerpt) VALUES (?, ?, ?, jsonb(?))
			ON CONFLICT (ns, id) DO UPDATE SET built_from = excluded.built_from, excerpt = excluded.excerpt
			RETURNING num`, ns, id, doc.commit, doc.excerpt).Scan(&num)
		if err != nil {
			return err
		}
		if doc.tokens != "" {
			_, err = tx.Exec("INSERT INTO fts (rowid, tokens) VALUES (?, ?)", num, doc.tokens)
			if err != nil {
				return err
			}
		}
	}

	return tx.Commit()
}
