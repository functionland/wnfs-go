package public

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"time"

	blocks "github.com/ipfs/go-block-format"
	blockservice "github.com/ipfs/go-blockservice"
	cid "github.com/ipfs/go-cid"
	cbornode "github.com/ipfs/go-ipld-cbor"
	golog "github.com/ipfs/go-log"
	base "github.com/functionland/wnfs-go/base"
)

var log = golog.Logger("wnfs")

type Header struct {
	Info     *Info
	Previous *cid.Cid // historical backpointer
	Merge    *cid.Cid // if this version is a merge, will be populated
	Metadata *cid.Cid
	Skeleton *cid.Cid // only present on directories
	Userland *cid.Cid
}

func loadHeader(ctx context.Context, bserv blockservice.BlockService, id cid.Cid) (*Header, error) {
	blk, err := bserv.GetBlock(ctx, id)
	if err != nil {
		return nil, err
	}

	return decodeHeaderBlock(blk)
}

func decodeHeaderBlock(blk blocks.Block) (*Header, error) {
	// TODO(b5): custom deserializer to avoid this double-decoding
	env := map[string]interface{}{}
	if err := cbornode.DecodeInto(blk.RawData(), &env); err != nil {
		return nil, err
	}

	nd, err := cbornode.DecodeBlock(blk)
	if err != nil {
		return nil, err
	}

	log.Debugw("decodeHeaderBlock", "info", env["info"], "env", env)

	info, ok := env["info"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("header block is missing info field")
	}
	h := &Header{
		Info: InfoFromMap(info),
	}

	for _, l := range nd.Links() {
		switch l.Name {
		case base.PreviousLinkName:
			h.Previous = &l.Cid
		case base.MergeLinkName:
			h.Merge = &l.Cid
		case base.MetadataLinkName:
			h.Metadata = &l.Cid
		case base.SkeletonLinkName:
			h.Skeleton = &l.Cid
		case base.UserlandLinkName:
			h.Userland = &l.Cid
		}
	}

	return h, nil
}

func (h *Header) encodeBlock() (blocks.Block, error) {
	LDFile := map[string]interface{}{
		"metadata": h.Metadata,
		"previous": h.Previous,
		"merge":    h.Merge,
		"skeleton": h.Skeleton,
		"userland": h.Userland,
	}

	if h.Info != nil {
		LDFile["info"] = h.Info.Map()
	}
	return cbornode.WrapObject(LDFile, base.DefaultMultihashType, -1)
}

func (h *Header) links() base.Links {
	links := base.NewLinks()

	if h.Previous != nil {
		links.Add(base.Link{Name: base.PreviousLinkName, Cid: *h.Previous})
	}
	if h.Merge != nil {
		links.Add(base.Link{Name: base.MergeLinkName, Cid: *h.Merge})
	}
	if h.Metadata != nil {
		links.Add(base.Link{Name: base.MetadataLinkName, Cid: *h.Metadata})
	}
	if h.Userland != nil {
		links.Add(base.Link{Name: base.UserlandLinkName, Cid: *h.Userland})
	}
	if h.Skeleton != nil {
		links.Add(base.Link{Name: base.SkeletonLinkName, Cid: *h.Skeleton})
	}

	return links
}

type Info struct {
	WNFS  base.SemVer   `json:"wnfs"`
	Type  base.NodeType `json:"type"`
	Mode  uint32        `json:"mode"`
	Ctime int64         `json:"ctime"`
	Mtime int64         `json:"mtime"`
	Size  int64         `json:"size"`
}

func NewInfo(t base.NodeType) *Info {
	ts := base.Timestamp().Unix()
	return &Info{
		WNFS:  base.LatestVersion,
		Type:  t,
		Mode:  base.ModeDefault,
		Ctime: ts,
		Mtime: ts,
		Size:  0,
	}
}

func (i *Info) Map() map[string]interface{} {
	return map[string]interface{}{
		"wnfs":  i.WNFS,
		"type":  i.Type,
		"mode":  i.Mode,
		"ctime": i.Ctime,
		"mtime": i.Mtime,
		"size":  i.Size,
	}
}

func InfoFromMap(m map[string]interface{}) *Info {
	i := &Info{}
	if version, ok := m["wnfs"].(string); ok {
		i.WNFS = base.SemVer(version)
	}
	if t, ok := m["type"].(int); ok {
		i.Type = base.NodeType(t)
	}
	if mode, ok := m["mode"].(int); ok {
		i.Mode = uint32(mode)
	}
	if ctime, ok := m["ctime"].(int); ok {
		i.Ctime = int64(ctime)
	}
	if mtime, ok := m["mtime"].(int); ok {
		i.Mtime = int64(mtime)
	}
	if size, ok := m["size"].(int); ok {
		i.Size = int64(size)
	}
	return i
}

type Tree struct {
	store Store  // embed a reference to store this tree is associated with
	name  string // directory name, used while linking
	cid   cid.Cid
	h     *Header

	metadata *LDFile
	skeleton Skeleton
	userland base.Links // links to files are stored in "userland" Header key
}

var (
	_ base.Tree             = (*Tree)(nil)
	_ base.WritableMetaNode = (*Tree)(nil)
	_ SkeletonSource        = (*Tree)(nil)
	_ fs.File               = (*Tree)(nil)
	_ base.FileInfo         = (*Tree)(nil)
	_ fs.ReadDirFile        = (*Tree)(nil)
)

func NewEmptyTree(store Store, name string) *Tree {
	return &Tree{
		store: store,
		name:  name,
		h: &Header{
			Info: NewInfo(base.NTDir),
		},

		userland: base.NewLinks(),
		skeleton: Skeleton{},
	}
}

func LoadTree(ctx context.Context, st Store, name string, id cid.Cid) (*Tree, error) {
	log.Debugw("loadTree", "name", name, "cid", id)

	h, err := loadHeader(ctx, st.Blockservice(), id)
	if err != nil {
		return nil, err
	}

	return treeFromHeader(ctx, st, h, name, id)
}

func treeFromHeader(ctx context.Context, store Store, h *Header, name string, id cid.Cid) (*Tree, error) {
	if h.Info.Type != base.NTDir {
		return nil, fmt.Errorf("expected file to be a tree")
	}
	if h.Skeleton == nil {
		return nil, fmt.Errorf("header is missing %s link", base.SkeletonLinkName)
	}
	sk, err := LoadSkeleton(ctx, store, *h.Skeleton)
	if err != nil {
		return nil, fmt.Errorf("loading %s data %s:\n%w", base.SkeletonLinkName, h.Skeleton, err)
	}

	if h.Userland == nil {
		return nil, fmt.Errorf("header is missing %s link", base.UserlandLinkName)
	}
	blk, err := store.Blockservice().GetBlock(ctx, *h.Userland)
	if err != nil {
		return nil, err
	}
	userland, err := base.DecodeLinksBlock(blk)
	if err != nil {
		return nil, fmt.Errorf("loading %s data %s:\n%w", base.UserlandLinkName, h.Userland, err)
	}

	return &Tree{
		store: store,
		name:  name,
		cid:   id,

		h: h,
		// metadata: md,
		skeleton: sk,
		userland: userland,
	}, nil
}

func (t *Tree) Links() base.Links          { return t.userland }
func (t *Tree) Raw() []byte                { return nil }
func (t *Tree) Name() string               { return t.name }
func (t *Tree) Size() int64                { return t.h.Info.Size }
func (t *Tree) ModTime() time.Time         { return time.Unix(t.h.Info.Ctime, 0) }
func (t *Tree) Mode() fs.FileMode          { return fs.FileMode(t.h.Info.Mode) }
func (t *Tree) Type() base.NodeType        { return t.h.Info.Type }
func (t *Tree) IsDir() bool                { return true }
func (t *Tree) Sys() interface{}           { return t.store }
func (t *Tree) Cid() cid.Cid               { return t.cid }
func (t *Tree) Stat() (fs.FileInfo, error) { return t, nil }

func (t *Tree) SetMetadata(md interface{}) error {
	t.metadata = NewLDFile(t.store, "", md)
	return nil
}

func (t *Tree) Metadata() (f base.LDFile, err error) {
	if t.metadata == nil && t.h.Metadata != nil {
		t.metadata, err = LoadLDFile(t.store.Context(), t.store, base.MetadataLinkName, *t.h.Metadata)
	}
	return t.metadata, err
}

func (t *Tree) Read(p []byte) (n int, err error) {
	return -1, errors.New("cannot read directory")
}
func (t *Tree) Close() error { return nil }

func (t *Tree) ReadDir(n int) ([]fs.DirEntry, error) {
	if n < 0 {
		n = t.userland.Len()
	}

	entries := make([]fs.DirEntry, 0, n)
	for i, link := range t.userland.SortedSlice() {
		entries = append(entries, base.NewFSDirEntry(link.Name, link.IsFile))

		if i == n {
			break
		}
	}
	return entries, nil
}

func (t *Tree) Skeleton() (Skeleton, error) {
	return t.skeleton, nil
}

func (t *Tree) Get(path base.Path) (fs.File, error) {
	ctx := context.TODO()
	head, tail := path.Shift()
	if head == "" {
		return t, nil
	}

	link := t.userland.Get(head)
	if link == nil {
		return nil, base.ErrNotFound
	}

	if tail != nil {
		ch, err := LoadTree(ctx, t.store, head, link.Cid)
		if err != nil {
			return nil, err
		}

		// recurse
		return ch.Get(tail)
	}

	return loadNode(ctx, t.store, link.Name, link.Cid)
}

func (t *Tree) AsHistoryEntry() base.HistoryEntry {
	return base.HistoryEntry{
		Cid:      t.cid,
		Previous: t.h.Previous,
		Size:     t.h.Info.Size,
		Type:     t.h.Info.Type,
		Mtime:    t.h.Info.Mtime,
	}
}

func (t *Tree) Mkdir(path base.Path) (res base.PutResult, err error) {
	if len(path) < 1 {
		return res, errors.New("invalid path: empty")
	}

	head, tail := path.Shift()
	childDir, err := t.getOrCreateDirectChildTree(head)
	if err != nil {
		return nil, err
	}

	if tail == nil {
		res, err = childDir.Put()
		if err != nil {
			return nil, err
		}
	} else {
		res, err = t.Mkdir(tail)
		if err != nil {
			return nil, err
		}
	}

	t.updateUserlandLink(head, res)
	return t.Put()
}

func (t *Tree) Add(path base.Path, f fs.File) (res base.PutResult, err error) {
	if len(path) == 0 {
		return res, errors.New("invalid path: empty")
	}

	head, tail := path.Shift()
	if tail == nil {
		res, err = t.createOrUpdateChildFile(head, f)
		if err != nil {
			return res, err
		}
	} else {
		childDir, err := t.getOrCreateDirectChildTree(head)
		if err != nil {
			return res, err
		}

		// recurse
		res, err = childDir.Add(tail, f)
		if err != nil {
			return res, err
		}
	}

	t.updateUserlandLink(head, res)
	// contents of tree have changed, write an update.
	// TODO(b5) - pretty sure this is a bug if multiple writes are batched in the
	// same "publish" / transaction. Write advances the previous / current CID,
	// so if the same directory is mutated multiple times before the next snapshot
	// we'll have intermediate states as the "previous" pointer
	return t.Put()
}

func (t *Tree) Copy(path base.Path, srcPathStr string, srcFS fs.FS) (res base.PutResult, err error) {
	log.Debugw("Tree.copy", "path", path, "srcPath", srcPathStr)
	if len(path) == 0 {
		return res, errors.New("invalid path: empty")
	}

	head, tail := path.Shift()
	if tail == nil {
		f, err := srcFS.Open(srcPathStr)
		if err != nil {
			return nil, err
		}

		res, err = t.createOrUpdateChild(srcPathStr, head, f, srcFS)
		if err != nil {
			return res, err
		}
	} else {
		childDir, err := t.getOrCreateDirectChildTree(head)
		if err != nil {
			return res, err
		}

		// recurse
		res, err = childDir.Copy(tail, srcPathStr, srcFS)
		if err != nil {
			return res, err
		}
	}

	t.updateUserlandLink(head, res)
	// contents of tree have changed, write an update.
	// TODO(b5) - pretty sure this is a bug if multiple writes are batched in the
	// same "publish" / transaction. Write advances the previous / current CID,
	// so if the same directory is mutated multiple times before the next snapshot
	// we'll have intermediate states as the "previous" pointer
	return t.Put()
}

func (t *Tree) Rm(path base.Path) (base.PutResult, error) {
	ctx := context.TODO()
	head, tail := path.Shift()
	if head == "" {
		return PutResult{}, fmt.Errorf("invalid path: empty")
	}

	if tail == nil {
		t.removeUserlandLink(head)
	} else {
		link := t.userland.Get(head)
		if link == nil {
			return PutResult{}, base.ErrNotFound
		}
		child, err := LoadTree(ctx, t.store, head, link.Cid)
		if err != nil {
			return nil, err
		}

		// recurse
		res, err := child.Rm(tail)
		if err != nil {
			return nil, err
		}
		t.updateUserlandLink(head, res)
	}

	// contents of tree have changed, write an update.
	return t.Put()
}

func (t *Tree) Put() (base.PutResult, error) {
	store := t.store
	ctx := context.TODO()

	blk, err := t.userland.EncodeBlock()
	if err != nil {
		return nil, err
	}
	if err = store.Blockservice().Blockstore().Put(ctx, blk); err != nil {
		return nil, err
	}
	id := blk.Cid()
	t.h.Userland = &id

	if t.metadata != nil {
		if _, err = t.metadata.Put(); err != nil {
			return nil, err
		}
		id := t.metadata.Cid()
		t.h.Metadata = &id
	}

	skf, err := t.skeleton.CBORFile()
	if err != nil {
		return nil, err
	}
	res, err := store.PutFile(skf)
	if err != nil {
		return nil, err
	}
	t.h.Skeleton = &res.Cid

	if t.cid.Defined() {
		// need to copy CID, as we're about to alter it's value
		id, _ := cid.Parse(t.cid)
		t.h.Previous = &id
	}

	if blk, err = t.h.encodeBlock(); err != nil {
		return PutResult{}, err
	}
	if err := t.store.Blockservice().Blockstore().Put(ctx, blk); err != nil {
		return PutResult{}, err
	}

	t.cid = blk.Cid()
	log.Debugw("wrote public tree", "name", t.name, "cid", t.cid.String(), "userlandLinkCount", t.userland.Len(), "size", t.h.Info.Size, "prev", t.h.Previous)

	return PutResult{
		Cid:  t.cid,
		Size: t.h.Info.Size,
		// Metadata: *t.h.Metadata,
		Userland: *t.h.Userland,
		Skeleton: t.skeleton,
	}, nil
}

func (t *Tree) History(ctx context.Context, max int) ([]base.HistoryEntry, error) {
	return history(ctx, t, max)
}

func history(ctx context.Context, n base.Node, max int) ([]base.HistoryEntry, error) {
	store, err := NodeStore(n)
	if err != nil {
		return nil, err
	}

	log := []base.HistoryEntry{
		n.AsHistoryEntry(),
	}

	prev := log[0].Previous
	for prev != nil {
		ent, err := loadHistoryEntry(ctx, store.Blockservice(), *prev)
		if err != nil {
			return nil, err
		}
		log = append(log, ent)
		prev = ent.Previous

		if len(log) == max {
			break
		}
	}

	return log, nil
}

func loadHistoryEntry(ctx context.Context, bserv blockservice.BlockService, id cid.Cid) (base.HistoryEntry, error) {
	h, err := loadHeader(ctx, bserv, id)
	if err != nil {
		return base.HistoryEntry{}, err
	}

	return base.HistoryEntry{
		Cid:      id,
		Previous: h.Previous,
		Type:     h.Info.Type,
		Mtime:    h.Info.Mtime,
		Size:     h.Info.Size,
	}, nil
}

func (t *Tree) getOrCreateDirectChildTree(name string) (*Tree, error) {
	ctx := context.TODO()
	link := t.userland.Get(name)
	if link == nil {
		return NewEmptyTree(t.store, name), nil
	}
	return LoadTree(ctx, t.store, name, link.Cid)
}

func (t *Tree) createOrUpdateChild(srcPathStr, name string, f fs.File, srcFS fs.FS) (base.PutResult, error) {
	if sdFile, ok := f.(base.LDFile); ok {
		return t.createOrUpdateChildLDFile(name, sdFile)
	}

	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if fi.IsDir() {
		return t.createOrUpdateChildDirectory(srcPathStr, name, f, srcFS)
	}
	return t.createOrUpdateChildFile(name, f)
}

func (t *Tree) createOrUpdateChildDirectory(srcPathStr, name string, f fs.File, srcFS fs.FS) (base.PutResult, error) {
	ctx := context.TODO()
	dir, ok := f.(fs.ReadDirFile)
	if !ok {
		return nil, fmt.Errorf("cannot read directory contents")
	}
	ents, err := dir.ReadDir(-1)
	if err != nil {
		return nil, fmt.Errorf("reading directory contents: %w", err)
	}

	var tree *Tree
	if link := t.userland.Get(name); link != nil {
		tree, err = LoadTree(ctx, t.store, name, link.Cid)
		if err != nil {
			return nil, err
		}
	} else {
		tree = NewEmptyTree(t.store, name)
	}

	var res base.PutResult
	for _, ent := range ents {
		res, err = tree.Copy(base.Path{ent.Name()}, filepath.Join(srcPathStr, ent.Name()), srcFS)
		if err != nil {
			return nil, err
		}
	}
	return res, nil
}

func (t *Tree) createOrUpdateChildLDFile(name string, sdf base.LDFile) (base.PutResult, error) {
	ctx := context.TODO()
	content, err := sdf.Data()
	if err != nil {
		return nil, err
	}

	if link := t.userland.Get(name); link != nil {
		prev, err := LoadLDFile(ctx, t.store, name, link.Cid)
		if err != nil {
			return nil, err
		}

		prev.content = content
		return prev.Put()
	}

	return NewLDFile(t.store, name, content).Put()
}

func (t *Tree) createOrUpdateChildFile(name string, f fs.File) (base.PutResult, error) {
	ctx := context.TODO()

	if sdFile, ok := f.(base.LDFile); ok {
		return t.createOrUpdateChildLDFile(name, sdFile)
	}

	if link := t.userland.Get(name); link != nil {
		previousFile, err := LoadFile(ctx, t.store, name, link.Cid)
		if err != nil {
			return nil, err
		}

		previousFile.SetFile(f)
		return previousFile.Put()
	}

	ch, err := NewFile(t.store, name, f)
	if err != nil {
		return nil, err
	}
	return ch.Put()
}

func (t *Tree) updateUserlandLink(name string, res base.PutResult) {
	log.Debugw("updateUserlandLink", "name", name, "cid", res.CID())
	t.userland.Add(res.ToLink(name))
	t.skeleton[name] = res.(PutResult).ToSkeletonInfo()
	t.h.Info.Mtime = base.Timestamp().Unix()
	t.h.Merge = nil // clear merge field in the case where we're mutating after a merge commit
}

func (t *Tree) removeUserlandLink(name string) {
	t.userland.Remove(name)
	delete(t.skeleton, name)
	t.h.Info.Mtime = base.Timestamp().Unix()
	t.h.Merge = nil // clear merge field in the case where we're mutating after a merge commit
}

type File struct {
	store Store
	name  string
	cid   cid.Cid
	h     *Header

	metadata *LDFile
	content  io.ReadCloser
}

var (
	_ base.Node             = (*Tree)(nil)
	_ base.WritableMetaNode = (*Tree)(nil)
	_ fs.File               = (*File)(nil)
	_ base.Node             = (*File)(nil)
)

func NewFile(store Store, name string, content io.ReadCloser) (*File, error) {
	var meta interface{}
	if mdn, ok := content.(base.Metadata); ok {
		md, err := mdn.Metadata()
		if err != nil {
			return nil, err
		}
		meta, err = md.Data()
		if err != nil {
			return nil, err
		}
	}

	return NewFileMetadata(store, name, content, meta)
}

func NewFileMetadata(store Store, name string, content io.ReadCloser, meta interface{}) (*File, error) {
	var md *LDFile
	if meta != nil {
		// need to construct a new file here to keep stores aligned
		md = NewBareLDFile(store, base.MetadataLinkName, meta)
	}

	return &File{
		store:    store,
		name:     name,
		content:  content,
		metadata: md,
		h: &Header{
			Info: NewInfo(base.NTFile),
		},
	}, nil
}

func LoadFile(ctx context.Context, store Store, name string, id cid.Cid) (*File, error) {
	h, err := loadHeader(ctx, store.Blockservice(), id)
	if err != nil {
		return nil, err
	}

	return fileFromHeader(ctx, store, h, name, id)
}

func fileFromHeader(ctx context.Context, store Store, h *Header, name string, id cid.Cid) (*File, error) {
	var (
		md  *LDFile
		err error
	)

	if h.Metadata != nil {
		if md, err = LoadLDFile(ctx, store, base.MetadataLinkName, *h.Metadata); err != nil {
			return nil, err
		}
	}

	if h.Userland == nil {
		return nil, errors.New("Header is missing 'userland' link")
	}

	return &File{
		store:    store,
		name:     name,
		cid:      id,
		h:        h,
		metadata: md,
	}, nil
}

func (f *File) Links() base.Links          { return base.NewLinks() }
func (f *File) Name() string               { return f.name }
func (f *File) Size() int64                { return f.h.Info.Size }
func (f *File) ModTime() time.Time         { return time.Unix(f.h.Info.Mtime, 0) }
func (f *File) Mode() fs.FileMode          { return fs.FileMode(f.h.Info.Mode) }
func (f *File) Type() base.NodeType        { return f.h.Info.Type }
func (f *File) IsDir() bool                { return false }
func (f *File) Sys() interface{}           { return f.store }
func (f *File) Cid() cid.Cid               { return f.cid }
func (f *File) Stat() (fs.FileInfo, error) { return f, nil }

func (f *File) SetMetadata(v interface{}) error {
	f.metadata = NewBareLDFile(f.store, base.MetadataLinkName, v)
	return nil
}

func (f *File) Metadata() (res base.LDFile, err error) {
	if f.metadata == nil {
		if f.h.Metadata == nil {
			return nil, base.ErrNoLink
		}

		f.metadata, err = LoadLDFile(f.store.Context(), f.store, base.MetadataLinkName, *f.h.Metadata)
	}
	return f.metadata, err
}

func (f *File) History(ctx context.Context, maxRevs int) ([]base.HistoryEntry, error) {
	return history(ctx, f, maxRevs)
}

func (f *File) Read(p []byte) (n int, err error) {
	f.ensureContent()
	return f.content.Read(p)
}

func (f *File) ensureContent() (err error) {
	if f.content == nil {
		ctx := f.store.Context()
		f.content, err = f.store.GetFile(ctx, *f.h.Userland)
	}
	return err
}

func (f *File) Close() error {
	if closer, ok := f.content.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

func (f *File) SetFile(r io.ReadCloser) {
	f.content = r

	if mdn, ok := r.(base.Metadata); ok {
		md, err := mdn.Metadata()
		if err != nil {
			return
		}
		meta, err := md.Data()
		if err != nil {
			return
		}
		f.metadata = NewBareLDFile(f.store, base.MetadataLinkName, meta)
	}
}

func (f *File) Put() (base.PutResult, error) {
	store := f.store
	ctx := context.TODO()

	userlandRes, err := store.PutFile(base.NewMemfileReader("", f.content))
	if err != nil {
		return PutResult{}, fmt.Errorf("putting file %q in store: %w", f.name, err)
	}
	f.h.Userland = &userlandRes.Cid

	if f.metadata != nil {
		log.Debugw("putting meta", "name", f.name)
		res, err := f.metadata.Put()
		if err != nil {
			return nil, err
		}
		id := res.CID()
		f.h.Metadata = &id
	}

	// add previous reference
	if f.cid.Defined() {
		f.h.Previous = &f.cid
	}

	blk, err := f.h.encodeBlock()
	if err != nil {
		return nil, err
	}
	f.cid = blk.Cid()
	if err := f.store.Blockservice().Blockstore().Put(ctx, blk); err != nil {
		return nil, err
	}

	log.Debugw("wrote public file Header", "name", f.name, "cid", f.cid.String(), "info", f.h)
	return PutResult{
		Cid:  f.cid,
		Size: f.h.Info.Size,
		// Metadata: *f.h.Metadata,
		Userland: *f.h.Userland,
		Type:     f.h.Info.Type,
	}, nil
}

func (f *File) AsHistoryEntry() base.HistoryEntry {
	return base.HistoryEntry{
		Cid:      f.cid,
		Previous: f.h.Previous,
		Mtime:    f.h.Info.Mtime,
		Type:     f.h.Info.Type,
		Size:     f.h.Info.Size,
	}
}

// load a public node
func loadNode(ctx context.Context, store Store, name string, id cid.Cid) (n base.Node, err error) {
	h, err := loadHeader(ctx, store.Blockservice(), id)
	if err != nil {
		return nil, err
	}

	if h.Info == nil {
		return nil, fmt.Errorf("not a valid wnfs Header: %q", id)
	}

	switch h.Info.Type {
	case base.NTFile:
		return fileFromHeader(ctx, store, h, name, id)
	case base.NTLDFile:
		blk, err := store.Blockservice().GetBlock(ctx, id)
		if err != nil {
			return nil, err
		}
		df := &LDFile{store: store, name: name, cid: id}
		return decodeLDFileBlock(df, blk)
	case base.NTDir:
		return treeFromHeader(ctx, store, h, name, id)
	default:
		return nil, fmt.Errorf("unrecognized node type: %s", h.Info.Type)
	}
}

func loadNodeFromSkeletonInfo(ctx context.Context, store Store, name string, info SkeletonInfo) (n base.Node, err error) {
	if info.IsFile {
		return LoadFile(ctx, store, name, info.Cid)
	}
	return LoadTree(ctx, store, name, info.Cid)
}

type LDFile struct {
	store Store
	name  string
	cid   cid.Cid
	bare  bool

	info        *Info
	metadata    *cid.Cid
	previous    *cid.Cid // historical backpointer
	content     interface{}
	jsonContent *bytes.Buffer
}

var (
	_ base.LDFile = (*LDFile)(nil)
	_ base.Node   = (*LDFile)(nil)
)

func NewLDFile(store Store, name string, content interface{}) *LDFile {
	return &LDFile{
		store:   store,
		name:    name,
		info:    NewInfo(base.NTLDFile),
		content: content,
	}
}

func NewBareLDFile(store Store, name string, content interface{}) *LDFile {
	return &LDFile{
		store:   store,
		name:    name,
		bare:    true,
		content: content,
	}
}

func LoadLDFile(ctx context.Context, store Store, name string, id cid.Cid) (*LDFile, error) {
	df := &LDFile{
		store: store,
		name:  name,
		cid:   id,
	}

	blk, err := store.Blockservice().GetBlock(ctx, id)
	if err != nil {
		return nil, err
	}

	return decodeLDFileBlock(df, blk)
}

func decodeLDFileBlock(df *LDFile, blk blocks.Block) (*LDFile, error) {
	// TODO(b5): this will choke on a data file with an array top level
	env := map[string]interface{}{}
	if err := cbornode.DecodeInto(blk.RawData(), &env); err != nil {
		return nil, err
	}

	// TODO (b5): links
	// nd, err := cbornode.DecodeBlock(blk)
	// if err != nil {
	// 	return nil, err
	// }
	// nd.Links()

	log.Debugw("decodeLDFileBlock", "info", env["info"], "env", env)

	if info, ok := env["info"].(map[string]interface{}); ok {
		df.info = InfoFromMap(info)
		if df.info.WNFS == "" {
			// info MUST have wnfs key present, otherwise it's considered a bare data file
			df.info = nil
			df.content = env
			return df, nil
		}
		df.content = env["content"]
		return df, nil
	}

	// if no info block exists, parse as a bare data file
	df.content = env
	df.bare = true

	return df, nil
}

func (df *LDFile) IsBare() bool      { return df.bare }
func (df *LDFile) Links() base.Links { return base.NewLinks() } // TODO(b5): remove Links method?
func (df *LDFile) Name() string      { return df.name }
func (df *LDFile) Size() int64 {
	if df.info != nil {
		return df.info.Size
	}
	return -1
}
func (df *LDFile) ModTime() time.Time {
	if df.info != nil {
		return time.Unix(df.info.Mtime, 0)
	}
	return time.Time{}
}
func (df *LDFile) Mode() fs.FileMode {
	if df.info != nil {
		return fs.FileMode(df.info.Mode)
	}
	return fs.FileMode(0)
}
func (df *LDFile) IsDir() bool                { return false }
func (df *LDFile) Sys() interface{}           { return df.store }
func (df *LDFile) Cid() cid.Cid               { return df.cid }
func (df *LDFile) Stat() (fs.FileInfo, error) { return df, nil }
func (df *LDFile) Data() (interface{}, error) { return df.content, nil }
func (df *LDFile) Type() base.NodeType        { return base.NTLDFile }
func (df *LDFile) ReadDir(n int) ([]fs.DirEntry, error) {
	return nil, fmt.Errorf("linked data file reading incomplete")
}

func (df *LDFile) History(ctx context.Context, maxRevs int) ([]base.HistoryEntry, error) {
	// TODO(b5): support history
	return nil, fmt.Errorf("data files don't yet support history")
	// return history(ctx, df, maxRevs)
}

func (df *LDFile) Read(p []byte) (n int, err error) {
	df.ensureContent()
	return df.jsonContent.Read(p)
}

func (df *LDFile) SetMetadata(m interface{}) error {
	// df.metadata = m
	return nil
}

func (df *LDFile) Metadata() (base.LDFile, error) {
	return nil, fmt.Errorf("unfinished: public.LDFile.Meta()")
}

func (df *LDFile) ensureContent() (err error) {
	if df.jsonContent == nil {
		buf := &bytes.Buffer{}
		// TODO(b5): use faster json lib
		if err := json.NewEncoder(buf).Encode(df.content); err != nil {
			return err
		}
		df.jsonContent = buf
	}
	return nil
}

func (df *LDFile) Close() error { return nil }

func (df *LDFile) SetFile(data interface{}) {
	df.content = data
	df.jsonContent = nil
}

func (df *LDFile) Put() (result base.PutResult, err error) {
	ctx := context.TODO()
	if df.bare {
		blk, err := cbornode.WrapObject(df.content, base.DefaultMultihashType, -1)
		if err != nil {
			return result, err
		}
		size, err := blk.Size()
		if err != nil {
			return result, err
		}
		if err = df.store.Blockservice().Blockstore().Put(ctx, blk); err != nil {
			return result, err
		}
		df.cid = blk.Cid()
		return PutResult{
			Cid:  blk.Cid(),
			Size: int64(size),

			Userland: blk.Cid(),
			Type:     base.NTLDFile,
		}, nil
	}

	if df.cid.Defined() {
		df.previous = &df.cid
	}
	if df.info == nil {
		df.info = &Info{}
	}

	blk, err := df.encodeBlock()
	if err != nil {
		return result, err
	}
	df.cid = blk.Cid()

	if err = df.store.Blockservice().Blockstore().Put(ctx, blk); err != nil {
		return result, err
	}

	log.Debugw("wrote public data file", "name", df.name, "cid", df.cid.String())
	return PutResult{
		Cid:      df.cid,
		Size:     df.info.Size,
		Userland: df.cid,
		Type:     df.info.Type,
	}, nil
}

func (df *LDFile) AsHistoryEntry() base.HistoryEntry {
	return base.HistoryEntry{
		Cid:      df.cid,
		Size:     df.info.Size,
		Type:     df.info.Type,
		Mtime:    df.info.Mtime,
		Previous: df.previous,
	}
}

func (df *LDFile) encodeBlock() (blocks.Block, error) {
	LDFile := map[string]interface{}{
		"metadata": df.metadata,
		"previous": df.previous,
		"content":  df.content,
	}
	if df.info != nil {
		LDFile["info"] = df.info.Map()
	}

	return cbornode.WrapObject(LDFile, base.DefaultMultihashType, -1)
}

func mergeResultToSkeletonInfo(mr base.MergeResult) SkeletonInfo {
	return SkeletonInfo{
		Cid:         mr.Cid,
		Userland:    mr.Userland,
		Metadata:    mr.Metadata,
		SubSkeleton: nil,
		IsFile:      mr.IsFile,
	}
}

type metaWrap struct {
	fs.File
	meta interface{}
}

func (m *metaWrap) SetMetadata(v interface{}) error {
	m.meta = v
	return nil
}

func (m *metaWrap) Metadata() (base.LDFile, error) {
	return NewLDFile(nil, base.MetadataLinkName, m.meta), nil
}

func WrapFileMetadata(f fs.File, meta interface{}) (wrapped fs.File) {
	if metaFile, ok := f.(base.WritableMetaNode); ok {
		metaFile.SetMetadata(meta)
		return f
	}

	log.Debugw("wrapping file in meta", "meta", meta)
	return &metaWrap{
		f,
		meta,
	}
}
