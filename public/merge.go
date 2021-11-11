package public

import (
	"fmt"
	"time"

	base "github.com/qri-io/wnfs-go/base"
	"github.com/qri-io/wnfs-go/mdstore"
)

func Merge(a, b base.Node) (result base.MergeResult, err error) {
	dest, err := base.NodeFS(a)
	if err != nil {
		return result, err
	}
	return merge(dest, a, b)
}

func merge(destFS base.MerkleDagFS, a, b base.Node) (result base.MergeResult, err error) {
	var (
		aCur, bCur   = a, b
		aHist, bHist = a.AsHistoryEntry(), b.AsHistoryEntry()
		aGen, bGen   = 0, 0
	)

	aStat, _ := a.Stat()
	bStat, _ := b.Stat()
	log.Debugf("merge afs: %#v, bfs: %#v\n", aStat.Sys(), bStat.Sys())

	// check for equality first
	if aHist.Cid.Equals(bHist.Cid) {
		return base.MergeResult{
			Type: base.MTInSync,
			Cid:  aHist.Cid,
			// Userland: aHist.Userland,
			// Metadata: bHist.Metadata,
			Size:   aHist.Size,
			IsFile: aHist.Metadata.IsFile,
		}, nil
	}

	afs, err := base.NodeFS(a)
	if err != nil {
		return result, err
	}
	bfs, err := base.NodeFS(b)
	if err != nil {
		return result, err
	}

	for {
		bCur = b
		bGen = 0
		bHist = b.AsHistoryEntry()
		for {
			if aHist.Cid.Equals(bHist.Cid) {
				if aGen == 0 && bGen > 0 {
					// fast-forward
					bHist = b.AsHistoryEntry()
					return base.MergeResult{
						Type: base.MTFastForward,
						// TODO(b5):
						// 	Userland: si.Cid,
						// 	Metadata: si.Metadata,
						Cid:    bHist.Cid,
						Size:   bHist.Size,
						IsFile: bHist.Metadata.IsFile,
					}, nil
				} else if aGen > 0 && bGen == 0 {
					result.Type = base.MTLocalAhead
					aHist := a.AsHistoryEntry()
					return base.MergeResult{
						Type:   base.MTLocalAhead,
						Cid:    aHist.Cid,
						Size:   aHist.Size,
						IsFile: aHist.Metadata.IsFile,
					}, nil
				} else {
					// both local & remote are greater than zero, have diverged
					merged, err := mergeNodes(destFS, a, b, aGen, bGen)
					if err != nil {
						return result, err
					}
					mergedStat, err := base.Stat(a)
					if err != nil {
						return result, err
					}
					return base.MergeResult{
						Type:   base.MTMergeCommit,
						Cid:    merged.Cid(),
						IsFile: !mergedStat.IsDir(),
					}, nil
				}
			}

			if bHist.Previous == nil {
				break
			}
			name, err := base.Filename(bCur)
			if err != nil {
				return result, err
			}
			bCur, err = loadNode(bfs, name, *bHist.Previous)
			if err != nil {
				return result, err
			}
			bHist = bCur.AsHistoryEntry()
			bGen++
		}

		if aHist.Previous == nil {
			break
		}
		name, err := base.Filename(aCur)
		if err != nil {
			return result, err
		}
		aCur, err = loadNode(afs, name, *aHist.Previous)
		if err != nil {
			return result, err
		}
		aHist = aCur.AsHistoryEntry()
		aGen++
	}

	// no common history, merge based on heigh & alpha-sorted-cid
	merged, err := mergeNodes(destFS, a, b, aGen, bGen)
	if err != nil {
		return result, err
	}
	mergedStat, err := base.Stat(a)
	if err != nil {
		return result, err
	}

	return base.MergeResult{
		Type:   base.MTMergeCommit,
		Cid:    merged.Cid(),
		IsFile: !mergedStat.IsDir(),
	}, nil
}

// 1. commits have diverged.
// 2. pick winner:
// 	* if "A" is winner "merge" value will be "B" head
// 	* if "B" is winner "merge value will be "A" head
// 	* in both cases the result itself to be a new CID
// 3. perform merge:
// 	* if both are directories, merge recursively
// 	* in all other cases, replace prior contents with winning CID
// always writes to a's filesystem
func mergeNodes(destFS base.MerkleDagFS, a, b base.Node, aGen, bGen int) (merged base.Node, err error) {
	log.Debugw("merge nodes", "aName", a.AsLink().Name, "bName", b.AsLink().Name, "destFS", fmt.Sprintf("%#v", destFS))
	// if b is preferred over a, switch values
	if aGen < bGen || (aGen == bGen && base.LessCID(b.Cid(), a.Cid())) {
		a, b = b, a
	}

	aTree, aIsTree := a.(*PublicTree)
	bTree, bIsTree := b.(*PublicTree)
	if aIsTree && bIsTree {
		return mergeTrees(destFS, aTree, bTree)
	}

	return mergeNode(destFS, a, b)
}

func mergeTrees(destFS base.MerkleDagFS, a, b *PublicTree) (*PublicTree, error) {
	log.Debugw("mergeTrees", "a_skeleton", a.skeleton)
	checked := map[string]struct{}{}

	for remName, remInfo := range b.skeleton {
		localInfo, existsLocally := a.skeleton[remName]
		log.Debugw("merging trees", "name", remName, "existsLocally", existsLocally)

		if !existsLocally {
			// remote has a file local is missing, add it.
			n, err := loadNodeFromSkeletonInfo(b.fs, remName, remInfo)
			if err != nil {
				return nil, err
			}
			log.Debugw("mergeTrees add file", "dir", a.Name(), "file", remName, "cid", n.Cid())

			if err := mdstore.CopyBlocks(destFS.Context(), n.Cid(), b.fs.DagStore(), destFS.DagStore()); err != nil {
				return nil, err
			}

			a.skeleton[remName] = remInfo
			a.userland.Add(n.AsLink())
			checked[remName] = struct{}{}
			continue
		}

		if localInfo.Cid.Equals(remInfo.Cid) {
			// both files are equal. no need to merge
			checked[remName] = struct{}{}
			continue
		}

		// node exists in both trees & CIDs are inequal. merge recursively
		lcl, err := loadNodeFromSkeletonInfo(a.fs, remName, localInfo)
		if err != nil {
			return nil, err
		}
		rem, err := loadNodeFromSkeletonInfo(b.fs, remName, remInfo)
		if err != nil {
			return nil, err
		}

		res, err := merge(destFS, lcl, rem)
		if err != nil {
			return nil, err
		}
		a.skeleton[remName] = res.ToSkeletonInfo()
		a.userland.Add(res.ToLink(remName))
		checked[remName] = struct{}{}
	}

	// iterate all of a's files making sure they're present on destFS
	for aName, aInfo := range a.skeleton {
		if _, ok := checked[aName]; !ok {
			log.Debugw("copying blocks for a file", "name", aName, "cid", aInfo.Cid)
			if err := mdstore.CopyBlocks(destFS.Context(), aInfo.Cid, a.fs.DagStore(), destFS.DagStore()); err != nil {
				return nil, err
			}
		}
	}

	a.h.Merge = &b.h.cid
	a.metadata.UnixMeta.Mtime = base.Timestamp().Unix()
	a.fs = destFS
	if _, err := a.Put(); err != nil {
		return nil, err
	}
	return a, nil
}

// construct a new node from a, with merge field set to b.Cid, store new node on
// dest
func mergeNode(destFS base.MerkleDagFS, a, b base.Node) (merged base.Node, err error) {
	bid := b.Cid()

	switch a := a.(type) {
	case *PublicTree:
		h := &header{
			Merge:    &bid,
			Previous: &a.h.cid,
			Size:     a.h.Size,
			Metadata: a.h.Metadata,
			Skeleton: a.h.Skeleton,
			Userland: a.h.Userland,
		}

		if err = h.write(destFS); err != nil {
			return nil, err
		}

		tree := &PublicTree{
			fs:   destFS,
			name: a.name,
			h:    h,

			metadata: a.metadata,
			skeleton: a.skeleton,
			userland: a.userland,
		}

		tree.metadata.UnixMeta.Mtime = time.Now().Unix()
		_, err := a.Put()
		return tree, err

	case *PublicFile:
		h := &header{
			Merge:    &bid,
			Previous: &a.h.cid,
			Size:     a.h.Size,
			Metadata: a.h.Metadata,
			Skeleton: a.h.Skeleton,
			Userland: a.h.Userland,
		}

		if err = h.write(destFS); err != nil {
			return nil, err
		}

		if err = a.ensureContent(); err != nil {
			return nil, err
		}

		return &PublicFile{
			fs:       destFS,
			name:     a.Name(),
			h:        h,
			metadata: a.metadata,
			content:  a.content,
		}, nil

		// if _, err = file.Put(); err != nil {
		// 	return nil, err
		// }

		// err = mdstore.CopyBlocks(dest.Context(), file.Cid(), a.fs.DagStore(), dest.DagStore())
		// return file, err
	default:
		return nil, fmt.Errorf("unknown type merging node %T", a)
	}
}
