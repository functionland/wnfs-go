package wnfs

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	cmp "github.com/google/go-cmp/cmp"
	golog "github.com/ipfs/go-log"
	base "github.com/functionland/wnfs-go/base"
	mockblocks "github.com/functionland/wnfs-go/mockblocks"
	private "github.com/functionland/wnfs-go/private"
	ratchet "github.com/functionland/wnfs-go/private/ratchet"
	public "github.com/functionland/wnfs-go/public"
	"github.com/stretchr/testify/assert"
	require "github.com/stretchr/testify/require"
)

var testRootKey Key = [32]byte{
	1, 2, 3, 4, 5, 6, 7, 8, 9, 0,
	1, 2, 3, 4, 5, 6, 7, 8, 9, 0,
	1, 2, 3, 4, 5, 6, 7, 8, 9, 0,
	1, 2,
}

func init() {
	if lvl := os.Getenv("WNFS_LOGGING"); lvl != "" {
		golog.SetLogLevel("wnfs", lvl)
	}
}

func TestTransactions(t *testing.T) {
	require := require.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := newMemTestStore(ctx, t)
	rs := ratchet.NewMemStore(ctx)

	fsys, err := NewEmptyFS(ctx, store.Blockservice(), rs, testRootKey)
	require.Nil(err)

	err = fsys.Write("public/hello.txt", base.NewMemfileBytes("hello.txt", []byte("hello")))
	require.Nil(err)
	res, err := fsys.Commit()
	require.Nil(err)

	_, err = FromCID(ctx, store.Blockservice(), rs, res.Root, *res.PrivateKey, *res.PrivateName)
	require.Nil(err)

	err = fsys.Write("public/goodbye.txt", base.NewMemfileBytes("goodbye.txt", []byte("goodbye")))
	require.Nil(err)
	err = fsys.Write("public/hello.txt", base.NewMemfileBytes("hello.txt", []byte("hello number two")))
	res, err = fsys.Commit()
	require.Nil(err)

	ents, err := fsys.History(ctx, "", -1)
	require.Nil(err)
	assert.Equal(t, 3, len(ents))

	_, err = FromCID(ctx, store.Blockservice(), rs, res.Root, *res.PrivateKey, *res.PrivateName)
	require.Nil(err)
}

func TestPublicWNFS(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	t.Run("writes_files", func(t *testing.T) {
		store := newMemTestStore(ctx, t)
		rs := ratchet.NewMemStore(ctx)

		fsys, err := NewEmptyFS(ctx, store.Blockservice(), rs, testRootKey)
		require.Nil(err)

		pathStr := "public/foo/hello.txt"
		fileContents := []byte("hello!")
		f := base.NewMemfileBytes("hello.txt", fileContents)

		err = fsys.Write(pathStr, f)
		require.Nil(err)
		_, err = fsys.Commit()
		require.Nil(err)

		t.Logf("wnfs root CID: %s", fsys.Cid())

		gotFileContents, err := fsys.Cat(pathStr)
		require.Nil(err)
		if diff := cmp.Diff(fileContents, gotFileContents); diff != "" {
			t.Errorf("result mismatch. (-want +got):\n%s", diff)
		}

		ents, err := fsys.Ls("public/foo")
		require.Nil(err)
		assert.Equal(len(ents), 1)

		err = fsys.Rm(pathStr)
		require.Nil(err)

		_, err = fsys.Cat(pathStr)
		require.ErrorIs(err, base.ErrNotFound)

		err = fsys.Mkdir("public/bar")
		require.Nil(err)

		ents, err = fsys.Ls("public/foo")
		require.Nil(err)
		assert.Equal(len(ents), 0)

		ents, err = fsys.Ls("public")
		require.Nil(err)
		assert.Equal(len(ents), 2)

		dfs := os.DirFS("./testdata")
		err = fsys.Cp("public/cats", "cats", dfs)
		require.Nil(err)

		ents, err = fsys.Ls("public/cats")
		require.Nil(err)
		assert.Equal(len(ents), 2)
	})
}

func TestMerge(t *testing.T) {
	require := require.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := newMemTestStore(ctx, t)
	rs := ratchet.NewMemStore(ctx)

	a, err := NewEmptyFS(ctx, store.Blockservice(), rs, testRootKey)
	if err != nil {
		t.Fatal(err)
	}

	pathStr := "public/foo/hello.txt"
	fileContents := []byte("hello!")
	f := base.NewMemfileBytes("hello.txt", fileContents)
	err = a.Write(pathStr, f)
	require.Nil(err)
	_, err = a.Commit()
	require.Nil(err)

	pn, err := a.PrivateName()
	require.Nil(err)

	b, err := FromCID(ctx, store.Blockservice(), rs, a.Cid(), a.RootKey(), pn)
	require.Nil(err)

	pathStr = "public/foo/world.txt"
	fileContents = []byte("world!")
	f = base.NewMemfileBytes("world.txt", fileContents)
	err = a.Write(pathStr, f)
	require.Nil(err)
	_, err = a.Commit()
	require.Nil(err)

	pathStr = "public/bonjour.txt"
	fileContents = []byte("bjr!")
	f = base.NewMemfileBytes("bonjour.txt", fileContents)
	err = b.Write(pathStr, f)
	require.Nil(err)
	_, err = a.Commit()
	require.Nil(err)

	err = Merge(ctx, a, b)
	require.Nil(err)
	res, err := a.Commit()
	require.Nil(err)

	t.Logf("%#v", res)
}

func BenchmarkPublicCat10MbFile(t *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rs := ratchet.NewMemStore(ctx)
	store, cleanup := newFileTestStore(ctx, t)
	defer cleanup()
	fsys, err := NewEmptyFS(ctx, store.Blockservice(), rs, testRootKey)
	if err != nil {
		t.Fatal(err)
	}

	data := make([]byte, 1024*10)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	textFile := base.NewMemfileBytes("bench.txt", data)
	err = fsys.Write("public/bench.txt", textFile)
	require.Nil(t, err)
	_, err = fsys.Commit()
	require.Nil(t, err)
	t.ResetTimer()

	for i := 0; i < t.N; i++ {
		if _, err := fsys.Cat("public/bench.txt"); err != nil {
			t.Fatal(err)
		}
	}
}

func BenchmarkPublicWrite10MbFile(t *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rs := ratchet.NewMemStore(ctx)
	store, cleanup := newFileTestStore(ctx, t)
	defer cleanup()
	fsys, err := NewEmptyFS(ctx, store.Blockservice(), rs, testRootKey)
	require.Nil(t, err)

	data := make([]byte, 1024*10)
	_, err = rand.Read(data)
	require.Nil(t, err)
	textFile := base.NewMemfileBytes("bench.txt", data)
	t.ResetTimer()

	for i := 0; i < t.N; i++ {
		fsys.Write("public/bench.txt", textFile)
		fsys.Commit()
	}
}

func BenchmarkPublicCat10MbFileSubdir(t *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rs := ratchet.NewMemStore(ctx)
	store, cleanup := newFileTestStore(ctx, t)
	defer cleanup()
	fsys, err := NewEmptyFS(ctx, store.Blockservice(), rs, testRootKey)
	if err != nil {
		t.Fatal(err)
	}

	data := make([]byte, 1024*10)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	textFile := base.NewMemfileBytes("bench.txt", data)
	err = fsys.Write("public/subdir/bench.txt", textFile)
	require.Nil(t, err)
	_, err = fsys.Commit()
	require.Nil(t, err)
	t.ResetTimer()

	for i := 0; i < t.N; i++ {
		if _, err := fsys.Cat("public/subdir/bench.txt"); err != nil {
			t.Fatal(err)
		}
	}
}

func BenchmarkPublicWrite10MbFileSubdir(t *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rs := ratchet.NewMemStore(ctx)
	store, cleanup := newFileTestStore(ctx, t)
	defer cleanup()
	fsys, err := NewEmptyFS(ctx, store.Blockservice(), rs, testRootKey)
	if err != nil {
		t.Fatal(err)
	}

	data := make([]byte, 1024*10)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	textFile := base.NewMemfileBytes("bench.txt", data)
	t.ResetTimer()

	for i := 0; i < t.N; i++ {
		fsys.Write("public/subdir/bench.txt", textFile)
		fsys.Commit()
	}
}

func BenchmarkPublicCp10DirectoriesWithOne10MbFileEach(t *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rs := ratchet.NewMemStore(ctx)
	store, cleanup := newFileTestStore(ctx, t)
	defer cleanup()
	fsys, err := NewEmptyFS(ctx, store.Blockservice(), rs, testRootKey)
	if err != nil {
		t.Fatal(err)
	}

	dir, err := ioutil.TempDir("", "bench_10_single_file_directories")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	for i := 0; i < 10; i++ {
		path := filepath.Join(dir, "copy_me", fmt.Sprintf("dir_%d", i))
		os.MkdirAll(path, 0755)
		path = filepath.Join(path, "bench.txt")

		data := make([]byte, 1024*10)
		if _, err := rand.Read(data); err != nil {
			t.Fatal(err)
		}
		ioutil.WriteFile(path, data, os.ModePerm)
	}

	dirFS := os.DirFS(dir)
	t.ResetTimer()

	for i := 0; i < t.N; i++ {
		fsys.Cp("public/copy_me", "copy_me", dirFS)
		fsys.Commit()
	}

	if _, err := fsys.Open("public/copy_me/dir_0/bench.txt"); err != nil {
		t.Fatal(err)
	}
}

func TestWNFSPrivate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rs := ratchet.NewMemStore(ctx)
	store, cleanup := newFileTestStore(ctx, t)
	defer cleanup()
	fsys, err := NewEmptyFS(ctx, store.Blockservice(), rs, testRootKey)
	if err != nil {
		t.Fatal(err)
	}

	pathStr := "private/foo/hello.txt"
	fileContents := []byte("hello!")
	f := base.NewMemfileBytes("hello.txt", fileContents)

	err = fsys.Write(pathStr, f)
	require.Nil(t, err)

	t.Logf("wnfs root CID: %s", fsys.Cid())

	gotFileContents, err := fsys.Cat(pathStr)
	require.Nil(t, err)

	if diff := cmp.Diff(fileContents, gotFileContents); diff != "" {
		t.Errorf("result mismatch. (-want +got):\n%s", diff)
	}

	ents, err := fsys.Ls("private/foo")
	require.Nil(t, err)
	assert.Equal(t, len(ents), 1)

	err = fsys.Rm(pathStr)
	require.Nil(t, err)

	_, err = fsys.Cat(pathStr)
	if !errors.Is(err, base.ErrNotFound) {
		t.Errorf("expected calling cat on removed path to return wrap of base.ErrNotFound. got: %s", err)
	}

	if err := fsys.Mkdir("private/bar"); err != nil {
		t.Error(err)
	}

	ents, err = fsys.Ls("private/foo")
	if err != nil {
		t.Error(err)
	}
	if len(ents) != 0 {
		t.Errorf("expected no entries. got: %d", len(ents))
	}

	ents, err = fsys.Ls("private")
	if err != nil {
		t.Error(err)
	}
	if len(ents) != 2 {
		t.Errorf("expected 2 entries. got: %d", len(ents))
	}

	dfs := os.DirFS("./testdata")
	err = fsys.Cp("private/cats", "cats", dfs)
	require.Nil(t, err)
	_, err = fsys.Commit()
	require.Nil(t, err)

	ents, err = fsys.Ls("private/cats")
	require.Nil(t, err)
	assert.Equal(t, len(ents), 2)

	// close context
	cancel()
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()

	key := fsys.RootKey()
	rootCid := fsys.Cid()
	pn, err := fsys.PrivateName()
	if err != nil {
		t.Fatal(err)
	}

	fsys, err = FromCID(ctx2, store.Blockservice(), rs, rootCid, key, pn)
	if err != nil {
		t.Fatal(err)
	}

	ents, err = fsys.Ls("private/cats")
	if err != nil {
		t.Error(err)
	}
	if len(ents) != 2 {
		t.Errorf("expected 2 entries. got: %d", len(ents))
	}
}

func BenchmarkPrivateCat10MbFile(t *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rs := ratchet.NewMemStore(ctx)
	store, cleanup := newFileTestStore(ctx, t)
	defer cleanup()
	fsys, err := NewEmptyFS(ctx, store.Blockservice(), rs, testRootKey)
	if err != nil {
		t.Fatal(err)
	}

	data := make([]byte, 1024*10)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	textFile := base.NewMemfileBytes("bench.txt", data)
	fsys.Write("private/bench.txt", textFile)
	fsys.Commit()
	t.ResetTimer()

	for i := 0; i < t.N; i++ {
		if _, err := fsys.Cat("private/bench.txt"); err != nil {
			t.Fatal(err)
		}
	}
}

func BenchmarkPrivateWrite10MbFile(t *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rs := ratchet.NewMemStore(ctx)
	store, cleanup := newFileTestStore(ctx, t)
	defer cleanup()
	fsys, err := NewEmptyFS(ctx, store.Blockservice(), rs, testRootKey)
	if err != nil {
		t.Fatal(err)
	}

	data := make([]byte, 1024*10)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	textFile := base.NewMemfileBytes("bench.txt", data)
	t.ResetTimer()

	for i := 0; i < t.N; i++ {
		fsys.Write("private/bench.txt", textFile)
		fsys.Commit()
	}
}

func BenchmarkPrivateCat10MbFileSubdir(t *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rs := ratchet.NewMemStore(ctx)
	store, cleanup := newFileTestStore(ctx, t)
	defer cleanup()
	fsys, err := NewEmptyFS(ctx, store.Blockservice(), rs, testRootKey)
	if err != nil {
		t.Fatal(err)
	}

	data := make([]byte, 1024*10)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	textFile := base.NewMemfileBytes("bench.txt", data)
	fsys.Write("private/subdir/bench.txt", textFile)
	fsys.Commit()
	t.ResetTimer()

	for i := 0; i < t.N; i++ {
		if _, err := fsys.Cat("private/subdir/bench.txt"); err != nil {
			t.Fatal(err)
		}
	}
}

func BenchmarkPrivateWrite10MbFileSubdir(t *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rs := ratchet.NewMemStore(ctx)
	store, cleanup := newFileTestStore(ctx, t)
	defer cleanup()
	fsys, err := NewEmptyFS(ctx, store.Blockservice(), rs, testRootKey)
	if err != nil {
		t.Fatal(err)
	}

	data := make([]byte, 1024*10)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	textFile := base.NewMemfileBytes("bench.txt", data)
	t.ResetTimer()

	for i := 0; i < t.N; i++ {
		fsys.Write("private/subdir/bench.txt", textFile)
		fsys.Commit()
	}
}

func BenchmarkPrivateCp10DirectoriesWithOne10MbFileEach(t *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rs := ratchet.NewMemStore(ctx)
	store, cleanup := newFileTestStore(ctx, t)
	defer cleanup()
	fsys, err := NewEmptyFS(ctx, store.Blockservice(), rs, testRootKey)
	if err != nil {
		t.Fatal(err)
	}

	dir, err := ioutil.TempDir("", "bench_10_single_file_directories")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	for i := 0; i < 10; i++ {
		path := filepath.Join(dir, "copy_me", fmt.Sprintf("dir_%d", i))
		os.MkdirAll(path, 0755)
		path = filepath.Join(path, "bench.txt")

		data := make([]byte, 1024*10)
		if _, err := rand.Read(data); err != nil {
			t.Fatal(err)
		}
		ioutil.WriteFile(path, data, os.ModePerm)
	}

	dirFS := os.DirFS(dir)
	t.ResetTimer()

	for i := 0; i < t.N; i++ {
		fsys.Cp("private/copy_me", "copy_me", dirFS)
		fsys.Commit()
	}

	if _, err := fsys.Open("private/copy_me/dir_0/bench.txt"); err != nil {
		t.Fatal(err)
	}
}

type fataler interface {
	Name() string
	Helper()
	Fatal(args ...interface{})
}

func newMemTestStore(ctx context.Context, f fataler) public.Store {
	f.Helper()
	return public.NewStore(ctx, mockblocks.NewOfflineMemBlockservice())
}

func newMemTestPrivateStore(ctx context.Context, f fataler) private.Store {
	f.Helper()
	rs := ratchet.NewMemStore(ctx)
	store, err := private.NewStore(ctx, mockblocks.NewOfflineMemBlockservice(), rs)
	if err != nil {
		f.Fatal(err)
	}
	return store
}

func newFileTestStore(ctx context.Context, f fataler) (st public.Store, cleanup func()) {
	f.Helper()
	bserv, cleanup, err := mockblocks.NewOfflineFileBlockservice(f.Name())
	if err != nil {
		f.Fatal(err)
	}

	store := public.NewStore(ctx, bserv)
	return store, cleanup
}
