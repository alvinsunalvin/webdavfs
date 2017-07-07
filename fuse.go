
package main

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"golang.org/x/net/context"
	"bazil.org/fuse"
	"bazil.org/fuse/fs"
)

const (
	attrValidTime  = 1 * time.Minute
	entryValidTime = 1 * time.Minute
)

type FS struct {
	root	*Node
}
var dav *DavClient

func attrSet(v fuse.SetattrValid, f fuse.SetattrValid) bool {
	return (v & f) > 0
}

func flagSet(v fuse.OpenFlags, f fuse.OpenFlags) bool {
	return (v & f) > 0
}

func NewFS(d *DavClient) *FS {
	dav = d
	return &FS{ root: rootNode }
}

func (fs *FS) Root() (fs.Node, error) {
	/*
	dnode, err := dav.Stat("/")
	if err == nil {
		fs.root.Dnode = dnode
	}
	*/
	return fs.root, nil
}

func (nd *Node) Mkdir(ctx context.Context, req *fuse.MkdirRequest) (ret fs.Node, err error) {
	nd.incMetaRefThenLock()
	path := joinPath(nd.getPath(), req.Name)
	nd.Unlock()
	err = dav.Mkcol(path + "/")
	nd.Lock()
	if err == nil {
		now := time.Now()
		nn := Dnode{
			Name: req.Name,
			Mtime: now,
			Ctime: now,
			IsDir: true,
		}
		n := nd.addNode(nn)
		n.Atime = now
		ret = n
	}
	nd.decMetaRef()
	nd.Unlock()
	return
}

func (nd *Node) Rename(ctx context.Context, req *fuse.RenameRequest, destDir fs.Node) (err error) {
	var lock1, lock2 *Node
	var oldPath, newPath string
	destNode := destDir.(*Node)
	first := true

	// Check if paths overlap. If so, only lock the
	// shortest path. If not, lock both.
	//
	// Need to do this in a loop, every time checking if this
	// condition still holds after both paths are locked.
	nd.Lock()
	for {
		srcDirPath := nd.getPath()
		dstDirPath := destNode.getPath()
		oldPath = joinPath(srcDirPath, req.OldName)
		newPath = joinPath(dstDirPath, req.NewName)

		var newLock1, newLock2 *Node
		if srcDirPath == dstDirPath {
			newLock1 = nd
		} else if strings.HasPrefix(srcDirPath, dstDirPath) {
			newLock1 = nd
		} else if strings.HasPrefix(dstDirPath, srcDirPath) {
			newLock1 = destNode
		} else {
			newLock1 = nd
			newLock2 = destNode
		}

		if !first {
			if lock1 == newLock1 && lock2 == newLock2 {
				break
			}
			lock1.decMetaRef()
			lock2.decMetaRef()
		}
		first = false

		lock1, lock2 = newLock1, newLock2
		lock1.incMetaRef()
		if lock2 != nil {
			lock2.incMetaRef()
		}
	}

	// Stat and if found, move
	nd.Unlock()
	dnode, err := dav.Stat(oldPath)
	if err == nil {
		if dnode.IsDir {
			oldPath += "/"
			newPath += "/"
		}
		err = dav.Move(oldPath, newPath)
	}
	nd.Lock()
	if err == nil {
		var n *Node
		if nd.Child != nil {
			n = nd.Child[req.OldName]
			delete(nd.Child, req.OldName)
		}
		if n != nil {
			// XXX FIXME check if destNode.Child[req.NewName]
			// already exists- if so we need to mark it deleted.
			if destNode.Child == nil {
				destNode.Child = map[string]*Node{}
			}
			destNode.Child[req.NewName] = n
			n.Name = req.NewName
		}
	}
	lock1.decMetaRef()
	if lock2 != nil {
		lock2.decMetaRef()
	}
	nd.Unlock()
	return
}

func (nd *Node) Remove(ctx context.Context, req *fuse.RemoveRequest) (err error) {
	nd.incMetaRefThenLock()
	path := joinPath(nd.getPath(), req.Name)
	nd.Unlock()
	props, err := dav.PropFindWithRedirect(path, 1, nil)
	if err == nil {
		if len(props) != 1 {
			if req.Dir {
				err = fuse.Errno(syscall.ENOTEMPTY)
			} else {
				err = fuse.EIO
			}
		}
		if err == nil {
			isDir := false
			for _, p := range props {
				if p.ResourceType == "collection" {
					isDir = true
				}
			}
			if req.Dir && !isDir {
				err = fuse.Errno(syscall.ENOTDIR)
			}
			if !req.Dir && isDir {
				err = fuse.Errno(syscall.EISDIR)
			}
		}
	}
	if err == nil {
		if req.Dir {
			path += "/"
		}
		err = dav.Delete(path)
	}
	nd.Lock()
	if err == nil && nd.Child != nil {
		n := nd.Child[req.Name]
		if n != nil {
			n.Deleted = true
			n.Name = n.Name + " (deleted)"
			fmt.Printf("DBG mark as deleted %s\n", n.Name)
			delete(nd.Child, req.Name)
		}
	}
	nd.decMetaRef()
	nd.Unlock()
	return
}

func (nd *Node) Attr(ctx context.Context, attr *fuse.Attr) (err error) {
	// XXX consider doing nothing if called within 1 second of Lookup().
	nd.incIoRef()
	fmt.Printf("Getattr %s (%s)\n", nd.Name, nd.getPath())
	dnode, err := dav.Stat(nd.getPath())
	if err == nil {
		if nd.Name != "" && dnode.IsDir != nd.IsDir {
			// XXX FIXME file changed to dir or vice versa ...
			// mark whole node stale, refuse i/o operations
			fmt.Printf("DBG huh isdir %v != isdir %v\n", dnode.IsDir, nd.IsDir)
			err = fuse.Errno(syscall.ESTALE)
		} else {
			mode := os.FileMode(0644)
			if dnode.IsDir {
				mode = os.FileMode(0755 | os.ModeDir)
			}
			if nd.Atime.Before(nd.Mtime) {
				nd.Atime = nd.Mtime
			}
			*attr = fuse.Attr{
				Valid: attrValidTime,
				Size: nd.Size,
				Blocks: (nd.Size + 511) / 512,
				Atime: nd.Atime,
				Mtime: nd.Mtime,
				Ctime: nd.Ctime,
				Crtime: nd.Ctime,
				Mode: os.FileMode(mode),
				Nlink: 1,
				Uid: 0,
				Gid: 0,
				BlockSize: 4096,
			}
			fmt.Printf("DBG return stat: %+v", attr)
		}
	} else {
		fmt.Printf("stat failed %v\n", err)
	}
	nd.decIoRef()
	return
}

func (nd *Node) Lookup(ctx context.Context, name string) (rn fs.Node, err error) {
	fmt.Printf("Lookup %s\n", name)
	nd.incIoRef()
	path := joinPath(nd.getPath(), name)
	dnode, err := dav.Stat(path)
	if err == nil {
		fmt.Printf("Lookup %s ok add %s\n", path, dnode.Name)
		rn = nd.addNode(dnode)
	} else {
		fmt.Printf("Lookup %s failed: %s\n", path, err)
	}
	nd.decIoRef()
	return
}

func (nd *Node) ReadDirAll(ctx context.Context) (dd []fuse.Dirent, err error) {
	nd.incIoRef()
	path := nd.getPath()
	dirs, err := dav.Readdir(path, false)
	nd.decIoRef()
	if err != nil {
		return
	}
	for _, d := range dirs {
		tp := fuse.DT_File
		if (d.IsDir) {
			tp =fuse.DT_Dir
		}
		dd = append(dd, fuse.Dirent{
			Name: d.Name,
			Inode: 0,
			Type: tp,
		})
	}
	return
}

func (nd *Node) Create(ctx context.Context, req *fuse.CreateRequest, resp *fuse.CreateResponse) (node fs.Node, handle fs.Handle, err error) {
	nd.incMetaRefThenLock()
	path := nd.getPath()
	nd.Unlock()
	trunc := flagSet(req.Flags, fuse.OpenTruncate)
	creat := flagSet(req.Flags, fuse.OpenCreate)
	read  := req.Flags.IsReadWrite() || req.Flags.IsReadOnly()
	write := req.Flags.IsReadWrite() || req.Flags.IsWriteOnly()
	excl  := flagSet(req.Flags, fuse.OpenExclusive)
	fmt.Printf("Create %s: trunc %v create %v read %v write %v excl %v\n", req.Name, trunc, creat, read, write, excl)
	path = joinPath(path, req.Name)
	created := false
	if trunc {
		created, err = dav.Put(path, []byte{})
	} else {
		created, err = dav.PutRange(path, []byte{}, 0)
	}
	if err == nil && excl && !created {
		err = fuse.EEXIST
	}
	if err == nil {
		dnode, err := dav.Stat(path)
		if err == nil {
			n := nd.addNode(dnode)
			node = n
			handle = n
		}
	}
	nd.Lock()
	nd.decMetaRef()
	nd.Unlock()
	return
}


func (nd *Node) Release(ctx context.Context, req *fuse.ReleaseRequest) error {
	// XXX FIXME add some sanity checks here-
	// see if refcnt == 0, subdirs are gone
	nd.Lock()
	fmt.Printf("Release %s\n", nd.getPath())
	if nd.Parent != nil {
		delete(nd.Parent.Child, nd.Name)
	}
	nd.Unlock()
	return nil
}

func (nd *Node) ftruncate(ctx context.Context, size uint64) (err error) {
	nd.incIoRef()
	path := nd.getPath()
	if size == 0 {
		if nd.Size > 0 {
			_, err = dav.Put(path, []byte{})
		}
	} else if size >= nd.Size {
		_, err = dav.PutRange(path, []byte{}, int64(size))
	} else {
		err = fuse.ERANGE
	}
	nd.decIoRef()
	return
}

func (nd *Node) Setattr(ctx context.Context, req *fuse.SetattrRequest, resp *fuse.SetattrResponse) (err error) {
	v := req.Valid
	if attrSet(v, fuse.SetattrMode) ||
	   attrSet(v, fuse.SetattrUid) ||
	   attrSet(v, fuse.SetattrGid) {
		return fuse.EPERM
	}

	if attrSet(v, fuse.SetattrSize) {
		fmt.Printf("Setattr %s: size %d\n", nd.Name, req.Size)
		err = nd.ftruncate(ctx, req.Size)
		if err != nil {
			return
		}
		nd.Lock()
		nd.Size = req.Size
		nd.Unlock()
	}

	nd.Lock()
	if attrSet(v, fuse.SetattrAtime) {
		nd.Atime = req.Atime
	}
	if attrSet(v, fuse.SetattrMtime){
		// XXX FIXME if req.Mtime is around "now", perhaps
		// do a 0-byte range put to "touch" the file.
		nd.Mtime = req.Mtime
	}
	attr := fuse.Attr{
		Valid: attrValidTime,
		Size:	nd.Size,
		Blocks:	nd.Size / 512,
		Atime: nd.Atime,
		Mtime: nd.Mtime,
		Ctime: nd.Ctime,
		Crtime: nd.Ctime,
		Mode: 0644,
		Nlink: 1,
		Uid: 0,
		Gid: 0,
		BlockSize: 4096,
	}
	resp.Attr = attr
	nd.Unlock()
	return
}

func (nf *Node) Fsync(ctx context.Context, req *fuse.FsyncRequest) error {
	return nil
}

func (nf *Node) Read(ctx context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) (err error) {
	if nf.Deleted {
		err = fuse.Errno(syscall.ESTALE)
		return
	}
	nf.incIoRef()
	path := nf.getPath()
	data, err := dav.GetRange(path, req.Offset, req.Size)
	if err == nil {
		resp.Data = data
	}
	nf.decIoRef()
	return
}

func (nf *Node) Write(ctx context.Context, req *fuse.WriteRequest, resp *fuse.WriteResponse) (err error) {
	if nf.Deleted {
		err = fuse.Errno(syscall.ESTALE)
		return
	}
	nf.incIoRef()
	path := nf.getPath()
	_, err = dav.PutRange(path, req.Data, req.Offset)
	if err == nil {
		resp.Size = len(req.Data)
		sz := uint64(req.Offset) + uint64(len(req.Data))
		nf.Lock()
		if sz > nf.Size {
			nf.Size = sz
		}
		nf.Unlock()
	}
	nf.decIoRef()
	return
}

func (nf *Node) Open(ctx context.Context, req *fuse.OpenRequest, resp *fuse.OpenResponse) (fs.Handle, error) {
	if nf.IsDir {
		return nf, nil
	}
	// truncate if we need to.
	trunc := flagSet(req.Flags, fuse.OpenTruncate)
	creat := flagSet(req.Flags, fuse.OpenCreate)
	read  := req.Flags.IsReadWrite() || req.Flags.IsReadOnly()
	write := req.Flags.IsReadWrite() || req.Flags.IsWriteOnly()
	fmt.Printf("Open %s: trunc %v create %v read %v write %v\n", nf.Name, trunc, creat, read, write)

	nf.incIoRef()
	path := nf.getPath()

	// See if cache is still valid.
	// XXX Update cache if we can.
	dnode, err := dav.Stat(path)
	if err == nil {
		if dnode.Size == nf.Size && dnode.Mtime == nf.Mtime {
			resp.Flags = fuse.OpenKeepCache
		}
	}
	err = nil

	// This is actually not called, truncating is
	// done by calling Setattr with 0 size.
	if trunc {
		_, err = dav.Put(path, []byte{})
		if err == nil {
			nf.Size = 0
		}
	}

	nf.decIoRef()

	if err != nil {
		return nil, err
	}
	return nf, nil
}
