package fs

import (
	"errors"
	"fmt"
	"os"

	"code.google.com/p/gogoprotobuf/proto"
	"github.com/boltdb/bolt"

	"bazil.org/bazil/cas"
	"bazil.org/bazil/cas/chunks"
	"bazil.org/bazil/fs/snap"
	wiresnap "bazil.org/bazil/fs/snap/wire"
	"bazil.org/bazil/fs/wire"
	"bazil.org/bazil/tokens"
	"bazil.org/bazil/util/env"
	"bazil.org/fuse"
	"bazil.org/fuse/fs"
)

type listSnaps struct {
	fs      *Volume
	rootDir *dir
}

var _ = fs.Node(&listSnaps{})
var _ = fs.NodeMkdirer(&listSnaps{})
var _ = fs.NodeStringLookuper(&listSnaps{})
var _ = fs.Handle(&listSnaps{})
var _ = fs.HandleReadDirer(&listSnaps{})

func (d *listSnaps) Attr() fuse.Attr {
	return fuse.Attr{
		Inode: tokens.InodeSnap,
		Mode:  os.ModeDir | 0755,
		Nlink: 1,
		Uid:   env.MyUID,
		Gid:   env.MyGID,
	}
}

var _ = fs.NodeStringLookuper(&listSnaps{})

func (d *listSnaps) Lookup(name string, intr fs.Intr) (fs.Node, fuse.Error) {
	var buf []byte
	err := d.fs.db.View(func(tx *bolt.Tx) error {
		bucket := d.fs.bucket(tx).Bucket(bucketSnap)
		if bucket == nil {
			return errors.New("snapshot bucket missing")
		}
		buf := bucket.Get([]byte(name))
		if buf == nil {
			return fuse.ENOENT
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var ref wire.SnapshotRef
	err = proto.Unmarshal(buf, &ref)
	if err != nil {
		return nil, fmt.Errorf("corrupt snapshot reference: %q: %v", name, err)
	}

	chunk, err := d.fs.chunkStore.Get(ref.Key, "snap", 0)
	if err != nil {
		return nil, fmt.Errorf("cannot fetch snapshot: %v", err)
	}

	var snapshot wiresnap.Snapshot
	err = proto.Unmarshal(chunk.Buf, &snapshot)
	if err != nil {
		return nil, fmt.Errorf("corrupt snapshot: %v: %v", ref.Key, err)
	}

	n, err := snap.Open(d.fs.chunkStore, &snapshot.Contents)
	if err != nil {
		return nil, fmt.Errorf("cannot serve snapshot: %v", err)
	}
	return n, nil
}

var _ = fs.NodeMkdirer(&listSnaps{})

// Mkdir takes a snapshot of this volume and records it under the
// given name.
func (d *listSnaps) Mkdir(req *fuse.MkdirRequest, intr fs.Intr) (fs.Node, fuse.Error) {
	var snapshot = wiresnap.Snapshot{
		Name: req.Name,
	}
	err := d.fs.db.View(func(tx *bolt.Tx) error {
		return d.rootDir.snapshot(tx, &snapshot.Contents, intr)
	})
	if err != nil {
		return nil, fmt.Errorf("cannot record snapshot: %v", err)
	}

	var key cas.Key
	{
		buf, err := proto.Marshal(&snapshot)
		if err != nil {
			return nil, fmt.Errorf("cannot marshal snapshot: %v", err)
		}
		if len(buf) == 0 {
			return nil, errors.New("marshaled snapshot become empty; this is a bug!")
		}

		// store the snapshot as a chunk, for disaster recovery
		key, err = d.fs.chunkStore.Add(&chunks.Chunk{
			Type:  "snap",
			Level: 0,
			Buf:   buf,
		})
		if err != nil {
			return nil, fmt.Errorf("cannot store snapshot: %v", err)
		}
	}

	var ref = wire.SnapshotRef{
		Key: key,
	}
	buf, err := proto.Marshal(&ref)
	if err != nil {
		return nil, fmt.Errorf("cannot marshal snapshot pointer: %v", err)
	}

	err = d.fs.db.Update(func(tx *bolt.Tx) error {
		b := d.fs.bucket(tx).Bucket(bucketSnap)
		if b == nil {
			return errors.New("snapshot bucket missing")
		}
		return b.Put([]byte(req.Name), buf)
	})

	n, err := snap.Open(d.fs.chunkStore, &snapshot.Contents)
	if err != nil {
		return nil, fmt.Errorf("cannot serve snapshot: %v", err)
	}
	return n, nil
}

var _ = fs.HandleReadDirer(&listSnaps{})

func (d *listSnaps) ReadDir(intr fs.Intr) ([]fuse.Dirent, fuse.Error) {
	// NOT HOLDING LOCKS, accessing database snapshot ONLY

	var entries []fuse.Dirent

	err := d.fs.db.View(func(tx *bolt.Tx) error {
		bucket := d.fs.bucket(tx).Bucket(bucketSnap)
		if bucket == nil {
			return errors.New("snapshot bucket missing")
		}
		c := bucket.Cursor()
		for k, _ := c.First(); k != nil; k, _ = c.Next() {
			fde := fuse.Dirent{
				Name: string(k),
				Type: fuse.DT_Dir,
			}
			entries = append(entries, fde)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}
