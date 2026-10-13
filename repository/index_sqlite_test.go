package repository

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testCommit = Hash("1111111111111111111111111111111111111111")

func TestSqliteIndex(t *testing.T) {
	excerpt := []byte(`{}`)

	newIndex := func(t *testing.T, path string) RepoIndex {
		t.Helper()
		repoIndex := NewSqliteIndex(path)
		t.Cleanup(func() { require.NoError(t, repoIndex.Close()) })
		return repoIndex
	}

	t.Run("search", func(t *testing.T) {
		idx, err := newIndex(t, ":memory:").GetIndex("a", 1)
		require.NoError(t, err)
		b := idx.NewBatch()
		require.NoError(t, b.Set("id1", testCommit, excerpt, []string{"the server crashes on login"}))
		require.NoError(t, b.Set("id2", testCommit, excerpt, []string{"login works, the server crashes later"}))
		require.NoError(t, b.Set("id3", testCommit, excerpt, []string{"-x +y title:z (abc"}))
		require.NoError(t, b.Set("id4", testCommit, excerpt, []string{"see file.go, and foo-bar"}))
		require.NoError(t, b.Set("id5", testCommit, excerpt, []string{"数据库连接失败"}))
		require.NoError(t, b.Set("id6", testCommit, excerpt, []string{"库存数据"}))
		require.NoError(t, b.Set("id7", testCommit, excerpt, []string{"first text ends with words", "_ second starts"}))
		require.NoError(t, b.Apply())

		tests := []struct {
			name     string
			phrases  []string
			expected []string
		}{
			{"no phrase", nil, nil},
			{"words are stemmed", []string{"crash"}, []string{"id1", "id2"}},
			{"a phrase", []string{"crashes on login"}, []string{"id1"}},
			{"a phrase in another order", []string{"login crashes"}, nil},
			{"any of the phrases", []string{"later", "foo"}, []string{"id2", "id4"}},
			{"a word that splits is a phrase", []string{"foo-bar"}, []string{"id4"}},
			{"a word that splits, in another order", []string{"bar-foo"}, nil},
			{"a file name", []string{"file.go"}, []string{"id4"}},
			{"a phrase doesn't span two texts", []string{"words second"}, nil},
			{"nor match their separator", []string{"words _ second"}, nil},
			{"a CJK word", []string{"数据库"}, []string{"id5"}},
			{"a shorter CJK word", []string{"数据"}, []string{"id5", "id6"}},
			{"a stop word alone", []string{"the"}, nil},
			{"a stop word and another phrase", []string{"the", "later"}, []string{"id2"}},
			{"no syntax: -", []string{"-x"}, []string{"id3"}},
			{"no syntax: +", []string{"+y"}, []string{"id3"}},
			{"no syntax: column", []string{"title:z"}, []string{"id3"}},
			{"no syntax: (", []string{"(abc"}, []string{"id3"}},
			{`no syntax: "`, []string{`"server`}, []string{"id1", "id2"}},
			{"no syntax: *", []string{"serv*"}, nil},
			{"no syntax: NEAR", []string{"NEAR(server login)"}, nil},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				res, err := idx.Search(tt.phrases)
				require.NoError(t, err)
				require.ElementsMatch(t, tt.expected, res)
			})
		}
	})

	t.Run("documents survive a reopen", func(t *testing.T) {
		repoIndex := newIndex(t, filepath.Join(t.TempDir(), "index.db"))

		idx, err := repoIndex.GetIndex("a", 1)
		require.NoError(t, err)
		b := idx.NewBatch()
		require.NoError(t, b.Set("id1", testCommit, []byte(`{"n":1}`), []string{"marker"}))
		require.NoError(t, b.Apply())
		require.NoError(t, repoIndex.Close())

		idx, err = repoIndex.GetIndex("a", 1)
		require.NoError(t, err)
		builtFrom, err := idx.BuiltFrom()
		require.NoError(t, err)
		require.Equal(t, map[string]Hash{"id1": testCommit}, builtFrom)
		commit, got, err := idx.Excerpt("id1")
		require.NoError(t, err)
		require.Equal(t, testCommit, commit)
		require.JSONEq(t, `{"n":1}`, string(got))
		res, err := idx.Search([]string{"marker"})
		require.NoError(t, err)
		require.Equal(t, []string{"id1"}, res)
	})

	t.Run("a database of another layout is emptied", func(t *testing.T) {
		repoIndex := newIndex(t, filepath.Join(t.TempDir(), "index.db"))

		idx, err := repoIndex.GetIndex("a", 1)
		require.NoError(t, err)
		b := idx.NewBatch()
		require.NoError(t, b.Set("id1", testCommit, excerpt, []string{"marker"}))
		require.NoError(t, b.Apply())
		_, err = idx.(*sqliteIndex).db.Exec("PRAGMA user_version = 0")
		require.NoError(t, err)
		require.NoError(t, repoIndex.Close())

		idx, err = repoIndex.GetIndex("a", 1)
		require.NoError(t, err)
		builtFrom, err := idx.BuiltFrom()
		require.NoError(t, err)
		require.Empty(t, builtFrom)

		// and the new one is kept across a reopen
		b = idx.NewBatch()
		require.NoError(t, b.Set("id1", testCommit, excerpt, []string{"marker"}))
		require.NoError(t, b.Apply())
		require.NoError(t, repoIndex.Close())
		idx, err = repoIndex.GetIndex("a", 1)
		require.NoError(t, err)
		res, err := idx.Search([]string{"marker"})
		require.NoError(t, err)
		require.Equal(t, []string{"id1"}, res)
	})

	t.Run("a file that isn't a database is replaced", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "index.db")
		require.NoError(t, os.WriteFile(path, bytes.Repeat([]byte("x"), 4096), 0o644))

		idx, err := newIndex(t, path).GetIndex("a", 1)
		require.NoError(t, err)
		b := idx.NewBatch()
		require.NoError(t, b.Set("id1", testCommit, excerpt, []string{"marker"}))
		require.NoError(t, b.Apply())
		res, err := idx.Search([]string{"marker"})
		require.NoError(t, err)
		require.Equal(t, []string{"id1"}, res)
		require.NoFileExists(t, path+".lock")
	})

	t.Run("a file that isn't a database is replaced once for all openers", func(t *testing.T) {
		const openers = 8
		path := filepath.Join(t.TempDir(), "index.db")
		require.NoError(t, os.WriteFile(path, bytes.Repeat([]byte("x"), 4096), 0o644))

		// all open the broken file at once, then all write once all are open: a
		// database deleted under one of them would lose its document
		idxs := make([]Index, openers)
		var opened sync.WaitGroup
		for n := range openers {
			opened.Go(func() {
				idx, err := newIndex(t, path).GetIndex("a", 1)
				assert.NoError(t, err)
				idxs[n] = idx
			})
		}
		opened.Wait()
		for n, idx := range idxs {
			require.NotNil(t, idx)
			b := idx.NewBatch()
			require.NoError(t, b.Set(fmt.Sprintf("id%d", n), testCommit, excerpt, nil))
			require.NoError(t, b.Apply())
		}

		idx, err := newIndex(t, path).GetIndex("a", 1)
		require.NoError(t, err)
		builtFrom, err := idx.BuiltFrom()
		require.NoError(t, err)
		require.Len(t, builtFrom, openers)
	})

	t.Run("a recovery lock left behind fails the recovery", func(t *testing.T) {
		timeout := sqliteRecoveryTimeout
		sqliteRecoveryTimeout = 50 * time.Millisecond
		t.Cleanup(func() { sqliteRecoveryTimeout = timeout })

		path := filepath.Join(t.TempDir(), "index.db")
		garbage := bytes.Repeat([]byte("x"), 4096)
		require.NoError(t, os.WriteFile(path, garbage, 0o644))
		require.NoError(t, os.WriteFile(path+".lock", nil, 0o644))

		_, err := newIndex(t, path).GetIndex("a", 1)
		require.ErrorContains(t, err, path+".lock")
		content, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, garbage, content)
	})

	t.Run("a document deleted by hand takes its tokens along", func(t *testing.T) {
		idx, err := newIndex(t, ":memory:").GetIndex("a", 1)
		require.NoError(t, err)
		b := idx.NewBatch()
		require.NoError(t, b.Set("id1", testCommit, excerpt, []string{"marker"}))
		require.NoError(t, b.Apply())

		_, err = idx.(*sqliteIndex).db.Exec("DELETE FROM docs WHERE id = 'id1'")
		require.NoError(t, err)

		// the next document gets the num of the deleted one, not its tokens
		b = idx.NewBatch()
		require.NoError(t, b.Set("id2", testCommit, excerpt, nil))
		require.NoError(t, b.Apply())
		res, err := idx.Search([]string{"marker"})
		require.NoError(t, err)
		require.Empty(t, res)
	})

	t.Run("a path with URI characters", func(t *testing.T) {
		dir := "a?b#c%41"
		if runtime.GOOS == "windows" {
			// Windows doesn't allow ? in a file name
			dir = "a#c%41"
		}
		path := filepath.Join(t.TempDir(), dir, "index.db")
		idx, err := newIndex(t, path).GetIndex("a", 1)
		require.NoError(t, err)
		b := idx.NewBatch()
		require.NoError(t, b.Set("id1", testCommit, excerpt, nil))
		require.NoError(t, b.Apply())
		require.FileExists(t, path)
	})

	t.Run("an excerpt that isn't an object is refused", func(t *testing.T) {
		idx, err := newIndex(t, ":memory:").GetIndex("a", 1)
		require.NoError(t, err)
		b := idx.NewBatch()
		require.Error(t, b.Set("id1", testCommit, []byte(`null`), nil))
		require.Error(t, b.Set("id1", testCommit, []byte(`[]`), nil))
		require.Error(t, b.Set("id1", testCommit, []byte(`{`), nil))
	})

	t.Run("a batch failing leaves nothing", func(t *testing.T) {
		repoIndex := newIndex(t, filepath.Join(t.TempDir(), "index.db"))

		idx, err := repoIndex.GetIndex("a", 1)
		require.NoError(t, err)
		b := idx.NewBatch()
		require.NoError(t, b.Set("id1", testCommit, excerpt, []string{"marker"}))
		// the database is closed underneath the batch
		require.NoError(t, repoIndex.Close())
		require.Error(t, b.Apply())

		idx, err = repoIndex.GetIndex("a", 1)
		require.NoError(t, err)
		builtFrom, err := idx.BuiltFrom()
		require.NoError(t, err)
		require.Empty(t, builtFrom)
	})
}

// TestSqliteIndexProcesses has two processes write to the same index at the
// same time: this test, and a copy of it as a child process.
func TestSqliteIndexProcesses(t *testing.T) {
	const docs = 50
	write := func(path, prefix string) error {
		repoIndex := NewSqliteIndex(path)
		defer repoIndex.Close()
		idx, err := repoIndex.GetIndex("a", 1)
		if err != nil {
			return err
		}
		for i := 0; i < docs; i++ {
			b := idx.NewBatch()
			err = b.Set(fmt.Sprintf("%s%d", prefix, i), testCommit, []byte(`{}`), []string{"marker"})
			if err != nil {
				return err
			}
			err = b.Apply()
			if err != nil {
				return err
			}
		}
		return nil
	}

	if path := os.Getenv("GIT_BUG_TEST_INDEX"); path != "" {
		require.NoError(t, os.WriteFile(path+".ready", nil, 0o644))
		require.NoError(t, write(path, "child"))
		return
	}

	path := filepath.Join(t.TempDir(), "index.db")
	repoIndex := NewSqliteIndex(path)
	t.Cleanup(func() { require.NoError(t, repoIndex.Close()) })
	idx, err := repoIndex.GetIndex("a", 1)
	require.NoError(t, err)

	// the parent holds the write lock until the child is about to write, for
	// the child to wait on it
	tx, err := idx.(*sqliteIndex).db.Begin()
	require.NoError(t, err)
	child := exec.Command(os.Args[0], "-test.run=^TestSqliteIndexProcesses$")
	child.Env = append(os.Environ(), "GIT_BUG_TEST_INDEX="+path)
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	require.NoError(t, child.Start())
	require.Eventually(t, func() bool {
		_, err := os.Stat(path + ".ready")
		return err == nil
	}, 10*time.Second, 10*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	require.NoError(t, tx.Commit())
	require.NoError(t, write(path, "parent"))
	require.NoError(t, child.Wait())

	// both see everything, the index open here before the child wrote included
	res, err := idx.Search([]string{"marker"})
	require.NoError(t, err)
	require.Len(t, res, 2*docs)
	builtFrom, err := idx.BuiltFrom()
	require.NoError(t, err)
	require.Len(t, builtFrom, 2*docs)
}
