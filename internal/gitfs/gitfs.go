// Code generated by golang.org/x/tools/cmd/bundle. DO NOT EDIT.
//go:generate bundle -o gitfs.go -prefix= golang.org/x/website/internal/gitfs

// Package gitfs presents a file tree downloaded from a remote Git repo as an in-memory fs.FS.
//

package gitfs

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	hashpkg "hash"
	"io"
	"io/fs"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

// A Hash is a SHA-1 Hash identifying a particular Git object.
type Hash [20]byte

func (h Hash) String() string { return fmt.Sprintf("%x", h[:]) }

// parseHash parses the (full-length) Git hash text.
func parseHash(text string) (Hash, error) {
	x, err := hex.DecodeString(text)
	if err != nil || len(x) != 20 {
		return Hash{}, fmt.Errorf("invalid hash")
	}
	var h Hash
	copy(h[:], x)
	return h, nil
}

// An objType is an object type indicator.
// The values are the ones used in Git pack encoding
// (https://git-scm.com/docs/pack-format#_object_types).
type objType int

const (
	objNone   objType = 0
	objCommit objType = 1
	objTree   objType = 2
	objBlob   objType = 3
	objTag    objType = 4
	// 5 undefined
	objOfsDelta objType = 6
	objRefDelta objType = 7
)

var objTypes = [...]string{
	objCommit: "commit",
	objTree:   "tree",
	objBlob:   "blob",
	objTag:    "tag",
}

func (t objType) String() string {
	if t < 0 || int(t) >= len(objTypes) || objTypes[t] == "" {
		return fmt.Sprintf("objType(%d)", int(t))
	}
	return objTypes[t]
}

// A dirEntry is a Git directory entry parsed from a tree object.
type dirEntry struct {
	mode int
	name []byte
	hash Hash
}

// parseDirEntry parses the next directory entry from data,
// returning the entry and the number of bytes it occupied.
// If data is malformed, parseDirEntry returns dirEntry{}, 0.
func parseDirEntry(data []byte) (dirEntry, int) {
	// Unclear where or if this format is documented by Git.
	// Each directory entry is an octal mode, then a space,
	// then a file name, then a NUL byte, then a 20-byte binary hash.
	// Note that 'git cat-file -p <treehash>' shows a textual representation
	// of this data, not the actual binary data. To see the binary data,
	// use 'echo <treehash> | git cat-file --batch | hexdump -C'.
	mode := 0
	i := 0
	for i < len(data) && data[i] != ' ' {
		c := data[i]
		if c < '0' || '7' < c {
			return dirEntry{}, 0
		}
		mode = mode*8 + int(c) - '0'
		i++
	}
	i++
	j := i
	for j < len(data) && data[j] != 0 {
		j++
	}
	if len(data)-j < 1+20 {
		return dirEntry{}, 0
	}
	name := data[i:j]
	var h Hash
	copy(h[:], data[j+1:])
	return dirEntry{mode, name, h}, j + 1 + 20
}

// treeLookup looks in the tree object data for the directory entry with the given name,
// returning the mode and hash associated with the name.
func treeLookup(data []byte, name string) (mode int, h Hash, ok bool) {
	// Note: The tree object directory entries are sorted by name,
	// but the directory entry data is not self-synchronizing,
	// so it's not possible to be clever and use a binary search here.
	for len(data) > 0 {
		e, size := parseDirEntry(data)
		if size == 0 {
			break
		}
		if string(e.name) == name {
			return e.mode, e.hash, true
		}
		data = data[size:]
	}
	return 0, Hash{}, false
}

// commitKeyValue parses the commit object data
// looking for the first header line "key: value" matching the given key.
// It returns the associated value.
// (Try 'git cat-file -p <commithash>' to see the commit data format.)
func commitKeyValue(data []byte, key string) ([]byte, bool) {
	for i := 0; i < len(data); i++ {
		if i == 0 || data[i-1] == '\n' {
			if data[i] == '\n' {
				break
			}
			if len(data)-i >= len(key)+1 && data[len(key)] == ' ' && string(data[:len(key)]) == key {
				val := data[len(key)+1:]
				for j := 0; j < len(val); j++ {
					if val[j] == '\n' {
						val = val[:j]
						break
					}
				}
				return val, true
			}
		}
	}
	return nil, false
}

// A store is a collection of Git objects, indexed for lookup by hash.
type store struct {
	sha1  hashpkg.Hash    // reused hash state
	index map[Hash]stored // lookup index
	data  []byte          // concatenation of all object data
}

// A stored describes a single stored object.
type stored struct {
	typ objType // object type
	off int     // object data is store.data[off:off+len]
	len int
}

// add adds an object with the given type and content to s, returning its Hash.
// If the object is already stored in s, add succeeds but doesn't store a second copy.
func (s *store) add(typ objType, data []byte) (Hash, []byte) {
	if s.sha1 == nil {
		s.sha1 = sha1.New()
	}

	// Compute Git hash for object.
	s.sha1.Reset()
	fmt.Fprintf(s.sha1, "%s %d\x00", typ, len(data))
	s.sha1.Write(data)
	var h Hash
	s.sha1.Sum(h[:0]) // appends into h

	e, ok := s.index[h]
	if !ok {
		if s.index == nil {
			s.index = make(map[Hash]stored)
		}
		e = stored{typ, len(s.data), len(data)}
		s.index[h] = e
		s.data = append(s.data, data...)
	}
	return h, s.data[e.off : e.off+e.len]
}

// object returns the type and data for the object with hash h.
// If there is no object with hash h, object returns 0, nil.
func (s *store) object(h Hash) (typ objType, data []byte) {
	d, ok := s.index[h]
	if !ok {
		return 0, nil
	}
	return d.typ, s.data[d.off : d.off+d.len]
}

// commit returns a treeFS for the file system tree associated with the given commit hash.
func (s *store) commit(h Hash) (*treeFS, error) {
	// The commit object data starts with key-value pairs
	typ, data := s.object(h)
	if typ == objNone {
		return nil, fmt.Errorf("commit %s: no such hash", h)
	}
	if typ != objCommit {
		return nil, fmt.Errorf("commit %s: unexpected type %s", h, typ)
	}
	treeHash, ok := commitKeyValue(data, "tree")
	if !ok {
		return nil, fmt.Errorf("commit %s: no tree", h)
	}
	h, err := parseHash(string(treeHash))
	if err != nil {
		return nil, fmt.Errorf("commit %s: invalid tree %q", h, treeHash)
	}
	return &treeFS{s, h}, nil
}

// A treeFS is an fs.FS serving a Git file system tree rooted at a given tree object hash.
type treeFS struct {
	s    *store
	tree Hash // root tree
}

// Open opens the given file or directory, implementing the fs.FS Open method.
func (t *treeFS) Open(name string) (f fs.File, err error) {
	defer func() {
		if e := recover(); e != nil {
			f = nil
			err = fmt.Errorf("gitfs panic: %v\n%s", e, debug.Stack())
		}
	}()

	// Process each element in the slash-separated path, producing hash identified by name.
	h := t.tree
	start := 0 // index of start of final path element in name
	if name != "." {
		for i := 0; i <= len(name); i++ {
			if i == len(name) || name[i] == '/' {
				// Look up name in current tree object h.
				typ, data := t.s.object(h)
				if typ != objTree {
					return nil, &fs.PathError{Path: name, Op: "open", Err: fs.ErrNotExist}
				}
				_, th, ok := treeLookup(data, name[start:i])
				if !ok {
					return nil, &fs.PathError{Path: name, Op: "open", Err: fs.ErrNotExist}
				}
				h = th
				if i < len(name) {
					start = i + 1
				}
			}
		}
	}

	// The hash h is the hash for name. Load its object.
	typ, data := t.s.object(h)
	info := fileInfo{name, name[start:], 0, 0}
	if typ == objBlob {
		// Regular file.
		info.mode = 0444
		info.size = int64(len(data))
		return &blobFile{info, bytes.NewReader(data)}, nil
	}
	if typ == objTree {
		// Directory.
		info.mode = fs.ModeDir | 0555
		return &dirFile{t.s, info, data, 0}, nil
	}
	return nil, &fs.PathError{Path: name, Op: "open", Err: fmt.Errorf("unexpected git object type %s", typ)}
}

// fileInfo implements fs.FileInfo.
type fileInfo struct {
	path string
	name string
	mode fs.FileMode
	size int64
}

func (i *fileInfo) Name() string { return i.name }

func (i *fileInfo) Type() fs.FileMode { return i.mode & fs.ModeType }

func (i *fileInfo) Mode() fs.FileMode { return i.mode }

func (i *fileInfo) Sys() interface{} { return nil }

func (i *fileInfo) IsDir() bool { return i.mode&fs.ModeDir != 0 }

func (i *fileInfo) Size() int64 { return i.size }

func (i *fileInfo) Info() (fs.FileInfo, error) { return i, nil }

func (i *fileInfo) ModTime() time.Time { return time.Time{} }

func (i *fileInfo) err(op string, err error) error {
	return &fs.PathError{Path: i.path, Op: op, Err: err}
}

// A blobFile implements fs.File for a regular file.
// The embedded bytes.Reader provides Read, Seek and other I/O methods.
type blobFile struct {
	info fileInfo
	*bytes.Reader
}

func (f *blobFile) Close() error { return nil }

func (f *blobFile) Stat() (fs.FileInfo, error) { return &f.info, nil }

// A dirFile implements fs.File for a directory.
type dirFile struct {
	s    *store
	info fileInfo
	data []byte
	off  int
}

func (f *dirFile) Close() error { return nil }

func (f *dirFile) Read([]byte) (int, error) { return 0, f.info.err("read", fs.ErrInvalid) }

func (f *dirFile) Stat() (fs.FileInfo, error) { return &f.info, nil }

func (f *dirFile) Seek(offset int64, whence int) (int64, error) {
	if offset == 0 && whence == 0 {
		// Allow rewind to start of directory.
		f.off = 0
		return 0, nil
	}
	return 0, f.info.err("seek", fs.ErrInvalid)
}

func (f *dirFile) ReadDir(n int) (list []fs.DirEntry, err error) {
	defer func() {
		if e := recover(); e != nil {
			list = nil
			err = fmt.Errorf("gitfs panic: %v\n%s", e, debug.Stack())
		}
	}()

	for (n <= 0 || len(list) < n) && f.off < len(f.data) {
		e, size := parseDirEntry(f.data[f.off:])
		if size == 0 {
			break
		}
		f.off += size
		typ, data := f.s.object(e.hash)
		mode := fs.FileMode(0444)
		if typ == objTree {
			mode = fs.ModeDir | 0555
		}
		infoSize := int64(0)
		if typ == objBlob {
			infoSize = int64(len(data))
		}
		name := string(e.name)
		list = append(list, &fileInfo{name, name, mode, infoSize})
	}
	if len(list) == 0 && n > 0 {
		return list, io.EOF
	}
	return list, nil
}

// A Repo is a connection to a remote repository served over HTTP or HTTPS.
type Repo struct {
	url  string // trailing slash removed
	caps map[string]string
}

// NewRepo connects to a Git repository at the given http:// or https:// URL.
func NewRepo(url string) (*Repo, error) {
	r := &Repo{url: strings.TrimSuffix(url, "/")}
	if err := r.handshake(); err != nil {
		return nil, err
	}
	return r, nil
}

// handshake runs the initial Git opening handshake, learning the capabilities of the server.
// See https://git-scm.com/docs/protocol-v2#_initial_client_request.
func (r *Repo) handshake() error {
	req, _ := http.NewRequest("GET", r.url+"/info/refs?service=git-upload-pack", nil)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Git-Protocol", "version=2")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("handshake: %v", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("handshake: %v\n%s", resp.Status, data)
	}
	if err != nil {
		return fmt.Errorf("handshake: reading body: %v", err)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-git-upload-pack-advertisement" {
		return fmt.Errorf("handshake: invalid response Content-Type: %v", ct)
	}

	pr := newPktLineReader(bytes.NewReader(data))
	lines, err := pr.Lines()
	if len(lines) == 1 && lines[0] == "# service=git-upload-pack" {
		lines, err = pr.Lines()
	}
	if err != nil {
		return fmt.Errorf("handshake: parsing response: %v", err)
	}
	caps := make(map[string]string)
	for _, line := range lines {
		verb, args, _ := strings.Cut(line, "=")
		caps[verb] = args
	}
	if _, ok := caps["version 2"]; !ok {
		return fmt.Errorf("handshake: not version 2: %q", lines)
	}
	r.caps = caps
	return nil
}

// Resolve looks up the given ref and returns the corresponding Hash.
func (r *Repo) Resolve(ref string) (Hash, error) {
	if h, err := parseHash(ref); err == nil {
		return h, nil
	}

	fail := func(err error) (Hash, error) {
		return Hash{}, fmt.Errorf("resolve %s: %v", ref, err)
	}
	refs, err := r.refs(ref)
	if err != nil {
		return fail(err)
	}
	for _, known := range refs {
		if known.name == ref {
			return known.hash, nil
		}
	}
	return fail(fmt.Errorf("unknown ref"))
}

// A ref is a single Git reference, like refs/heads/main, refs/tags/v1.0.0, or HEAD.
type ref struct {
	name string // "refs/heads/main", "refs/tags/v1.0.0", "HEAD"
	hash Hash   // hexadecimal hash
}

// refs executes an ls-refs command on the remote server
// to look up refs with the given prefixes.
// See https://git-scm.com/docs/protocol-v2#_ls_refs.
func (r *Repo) refs(prefixes ...string) ([]ref, error) {
	if _, ok := r.caps["ls-refs"]; !ok {
		return nil, fmt.Errorf("refs: server does not support ls-refs")
	}

	var buf bytes.Buffer
	pw := newPktLineWriter(&buf)
	pw.WriteString("command=ls-refs")
	pw.Delim()
	pw.WriteString("peel")
	pw.WriteString("symrefs")
	for _, prefix := range prefixes {
		pw.WriteString("ref-prefix " + prefix)
	}
	pw.Close()
	postbody := buf.Bytes()

	req, _ := http.NewRequest("POST", r.url+"/git-upload-pack", &buf)
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	req.Header.Set("Accept", "application/x-git-upload-pack-result")
	req.Header.Set("Git-Protocol", "version=2")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("refs: %v", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("refs: %v\n%s", resp.Status, data)
	}
	if err != nil {
		return nil, fmt.Errorf("refs: reading body: %v", err)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-git-upload-pack-result" {
		return nil, fmt.Errorf("refs: invalid response Content-Type: %v", ct)
	}

	var refs []ref
	lines, err := newPktLineReader(bytes.NewReader(data)).Lines()
	if err != nil {
		return nil, fmt.Errorf("refs: parsing response: %v %d\n%s\n%s", err, len(data), hex.Dump(postbody), hex.Dump(data))
	}
	for _, line := range lines {
		hash, rest, ok := strings.Cut(line, " ")
		if !ok {
			return nil, fmt.Errorf("refs: parsing response: invalid line: %q", line)
		}
		h, err := parseHash(hash)
		if err != nil {
			return nil, fmt.Errorf("refs: parsing response: invalid line: %q", line)
		}
		name, _, _ := strings.Cut(rest, " ")
		refs = append(refs, ref{hash: h, name: name})
	}
	return refs, nil
}

// Clone resolves the given ref to a hash and returns the corresponding fs.FS.
func (r *Repo) Clone(ref string) (Hash, fs.FS, error) {
	fail := func(err error) (Hash, fs.FS, error) {
		return Hash{}, nil, fmt.Errorf("clone %s: %v", ref, err)
	}
	h, err := r.Resolve(ref)
	if err != nil {
		return fail(err)
	}
	tfs, err := r.fetch(h)
	if err != nil {
		return fail(err)
	}
	return h, tfs, nil
}

// CloneHash returns the fs.FS for the given hash.
func (r *Repo) CloneHash(h Hash) (fs.FS, error) {
	tfs, err := r.fetch(h)
	if err != nil {
		return nil, fmt.Errorf("clone %s: %v", h, err)
	}
	return tfs, nil
}

// fetch returns the fs.FS for a given hash.
func (r *Repo) fetch(h Hash) (fs.FS, error) {
	// Fetch a shallow packfile from the remote server.
	// Shallow means it only contains the tree at that one commit,
	// not the entire history of the repo.
	// See https://git-scm.com/docs/protocol-v2#_fetch.
	opts, ok := r.caps["fetch"]
	if !ok {
		return nil, fmt.Errorf("fetch: server does not support fetch")
	}
	if !strings.Contains(" "+opts+" ", " shallow ") {
		return nil, fmt.Errorf("fetch: server does not support shallow fetch")
	}

	// Prepare and send request for pack file.
	var buf bytes.Buffer
	pw := newPktLineWriter(&buf)
	pw.WriteString("command=fetch")
	pw.Delim()
	pw.WriteString("deepen 1")
	pw.WriteString("want " + h.String())
	pw.WriteString("done")
	pw.Close()
	postbody := buf.Bytes()

	req, _ := http.NewRequest("POST", r.url+"/git-upload-pack", &buf)
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	req.Header.Set("Accept", "application/x-git-upload-pack-result")
	req.Header.Set("Git-Protocol", "version=2")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		data, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("fetch: %v\n%s\n%s", resp.Status, data, hex.Dump(postbody))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-git-upload-pack-result" {
		return nil, fmt.Errorf("fetch: invalid response Content-Type: %v", ct)
	}

	// Response is sequence of pkt-line packets.
	// It is plain text output (printed by git) until we find "packfile".
	// Then it switches to packets with a single prefix byte saying
	// what kind of data is in that packet:
	// 1 for pack file data, 2 for text output, 3 for errors.
	var data []byte
	pr := newPktLineReader(resp.Body)
	sawPackfile := false
	for {
		line, err := pr.Next()
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("fetch: parsing response: %v", err)
		}
		if line == nil { // ignore delimiter
			continue
		}
		if !sawPackfile {
			// Discard response lines until we get to packfile start.
			if strings.TrimSuffix(string(line), "\n") == "packfile" {
				sawPackfile = true
			}
			continue
		}
		if len(line) == 0 || line[0] == 0 || line[0] > 3 {
			fmt.Printf("%q\n", line)
			continue
			return nil, fmt.Errorf("fetch: malformed response: invalid sideband: %q", line)
		}
		switch line[0] {
		case 1:
			data = append(data, line[1:]...)
		case 2:
			fmt.Printf("%s\n", line[1:])
		case 3:
			return nil, fmt.Errorf("fetch: server error: %s", line[1:])
		}
	}

	if !bytes.HasPrefix(data, []byte("PACK")) {
		return nil, fmt.Errorf("fetch: malformed response: not packfile")
	}

	// Unpack pack file and return fs.FS for the commit we downloaded.
	var s store
	if err := unpack(&s, data); err != nil {
		return nil, fmt.Errorf("fetch: %v", err)
	}
	tfs, err := s.commit(h)
	if err != nil {
		return nil, fmt.Errorf("fetch: %v", err)
	}
	return tfs, nil
}

// unpack parses data, which is a Git pack-formatted archive,
// writing every object it contains to the store s.
//
// See https://git-scm.com/docs/pack-format for format documentation.
func unpack(s *store, data []byte) error {
	// If the store is empty, pre-allocate the length of data.
	// This should be about the right order of magnitude for the eventual data,
	// avoiding many growing steps during append.
	if len(s.data) == 0 {
		s.data = make([]byte, 0, len(data))
	}

	// Pack data starts with 12-byte header: "PACK" version[4] nobj[4].
	if len(data) < 12+20 {
		return fmt.Errorf("malformed git pack: too short")
	}
	hdr := data[:12]
	vers := binary.BigEndian.Uint32(hdr[4:8])
	nobj := binary.BigEndian.Uint32(hdr[8:12])
	if string(hdr[:4]) != "PACK" || vers != 2 && vers != 3 || len(data) < 12+20 || int64(nobj) >= int64(len(data)) {
		return fmt.Errorf("malformed git pack")
	}
	if vers == 3 {
		return fmt.Errorf("cannot read git pack v3")
	}

	// Pack data ends with SHA1 of the entire pack.
	sum := sha1.Sum(data[:len(data)-20])
	if !bytes.Equal(sum[:], data[len(data)-20:]) {
		return fmt.Errorf("malformed git pack: bad checksum")
	}

	// Object data is everything between hdr and ending SHA1.
	// Unpack every object into the store.
	objs := data[12 : len(data)-20]
	off := 0
	for i := 0; i < int(nobj); i++ {
		_, _, _, encSize, err := unpackObject(s, objs, off)
		if err != nil {
			return fmt.Errorf("unpack: malformed git pack: %v", err)
		}
		off += encSize
	}
	if off != len(objs) {
		return fmt.Errorf("malformed git pack: junk after objects")
	}
	return nil
}

// unpackObject unpacks the object at objs[off:] and writes it to the store s.
// It returns the type, hash, and content of the object, as well as the encoded size,
// meaning the number of bytes at the start of objs[off:] that this record occupies.
func unpackObject(s *store, objs []byte, off int) (typ objType, h Hash, content []byte, encSize int, err error) {
	fail := func(err error) (objType, Hash, []byte, int, error) {
		return 0, Hash{}, nil, 0, err
	}
	if off < 0 || off >= len(objs) {
		return fail(fmt.Errorf("invalid object offset"))
	}

	// Object starts with varint-encoded type and length n.
	// (The length n is the length of the compressed data that follows,
	// not the length of the actual object.)
	u, size := binary.Uvarint(objs[off:])
	if size <= 0 {
		return fail(fmt.Errorf("invalid object: bad varint header"))
	}
	typ = objType((u >> 4) & 7)
	n := int(u&15 | u>>7<<4)

	// Git often stores objects that differ very little (different revs of a file).
	// It can save space by encoding one as "start with this other object and apply these diffs".
	// There are two ways to specify "this other object": an object ref (20-byte SHA1)
	// or as a relative offset to an earlier position in the objs slice.
	// For either of these, we need to fetch the other object's type and data (deltaTyp and deltaBase).
	// The Git docs call this the "deltified representation".
	var deltaTyp objType
	var deltaBase []byte
	switch typ {
	case objRefDelta:
		if len(objs)-(off+size) < 20 {
			return fail(fmt.Errorf("invalid object: bad delta ref"))
		}
		// Base block identified by SHA1 of an already unpacked hash.
		var h Hash
		copy(h[:], objs[off+size:])
		size += 20
		deltaTyp, deltaBase = s.object(h)
		if deltaTyp == 0 {
			return fail(fmt.Errorf("invalid object: unknown delta ref %v", h))
		}

	case objOfsDelta:
		i := off + size
		if len(objs)-i < 20 {
			return fail(fmt.Errorf("invalid object: too short"))
		}
		// Base block identified by relative offset to earlier position in objs,
		// using a varint-like but not-quite-varint encoding.
		// Look for "offset encoding:" in https://git-scm.com/docs/pack-format.
		d := int64(objs[i] & 0x7f)
		for objs[i]&0x80 != 0 {
			i++
			if i-(off+size) > 10 {
				return fail(fmt.Errorf("invalid object: malformed delta offset"))
			}
			d = d<<7 | int64(objs[i]&0x7f)
			d += 1 << 7
		}
		i++
		size = i - off

		// Re-unpack the object at the earlier offset to find its type and content.
		if d == 0 || d > int64(off) {
			return fail(fmt.Errorf("invalid object: bad delta offset"))
		}
		var err error
		deltaTyp, _, deltaBase, _, err = unpackObject(s, objs, off-int(d))
		if err != nil {
			return fail(fmt.Errorf("invalid object: bad delta offset"))
		}
	}

	// The main encoded data is a zlib-compressed stream.
	br := bytes.NewReader(objs[off+size:])
	zr, err := zlib.NewReader(br)
	if err != nil {
		return fail(fmt.Errorf("invalid object deflate: %v", err))
	}
	data, err := io.ReadAll(zr)
	if err != nil {
		return fail(fmt.Errorf("invalid object: bad deflate: %v", err))
	}
	if len(data) != n {
		return fail(fmt.Errorf("invalid object: deflate size %d != %d", len(data), n))
	}
	encSize = len(objs[off:]) - br.Len()

	// If we fetched a base object above, the stream is an encoded delta.
	// Otherwise it is the raw data.
	switch typ {
	default:
		return fail(fmt.Errorf("invalid object: unknown object type"))
	case objCommit, objTree, objBlob, objTag:
		// ok
	case objRefDelta, objOfsDelta:
		// Actual object type is the type of the base object.
		typ = deltaTyp

		// Delta encoding starts with size of base object and size of new object.
		baseSize, s := binary.Uvarint(data)
		data = data[s:]
		if baseSize != uint64(len(deltaBase)) {
			return fail(fmt.Errorf("invalid object: mismatched delta src size"))
		}
		targSize, s := binary.Uvarint(data)
		data = data[s:]

		// Apply delta to base object, producing new object.
		targ := make([]byte, targSize)
		if err := applyDelta(targ, deltaBase, data); err != nil {
			return fail(fmt.Errorf("invalid object: %v", err))
		}
		data = targ
	}

	h, data = s.add(typ, data)
	return typ, h, data, encSize, nil
}

// applyDelta applies the delta encoding to src, producing dst,
// which has already been allocated to the expected final size.
// See https://git-scm.com/docs/pack-format#_deltified_representation for docs.
func applyDelta(dst, src, delta []byte) error {
	for len(delta) > 0 {
		// Command byte says what comes next.
		cmd := delta[0]
		delta = delta[1:]
		switch {
		case cmd == 0:
			// cmd == 0 is reserved.
			return fmt.Errorf("invalid delta cmd")

		case cmd&0x80 != 0:
			// Copy from base object, 4-byte offset, 3-byte size.
			// But any zero byte in the offset or size can be omitted.
			// The bottom 7 bits of cmd say which offset/size bytes are present.
			var off, size int64
			for i := uint(0); i < 4; i++ {
				if cmd&(1<<i) != 0 {
					off |= int64(delta[0]) << (8 * i)
					delta = delta[1:]
				}
			}
			for i := uint(0); i < 3; i++ {
				if cmd&(0x10<<i) != 0 {
					size |= int64(delta[0]) << (8 * i)
					delta = delta[1:]
				}
			}
			// Size 0 means size 0x10000 for some reason. (!)
			if size == 0 {
				size = 0x10000
			}
			copy(dst[:size], src[off:off+size])
			dst = dst[size:]

		default:
			// Up to 0x7F bytes of literal data, length in bottom 7 bits of cmd.
			n := int(cmd)
			copy(dst[:n], delta[:n])
			dst = dst[n:]
			delta = delta[n:]
		}
	}
	if len(dst) != 0 {
		return fmt.Errorf("delta encoding too short")
	}
	return nil
}

// A pktLineReader reads Git pkt-line-formatted packets.
//
// Each n-byte packet is preceded by a 4-digit hexadecimal length
// encoding n+4 (the length counts its own bytes), like "0006a\n" for "a\n".
//
// A packet starting with 0000 is a so-called flush packet.
// A packet starting with 0001 is a delimiting marker,
// which usually marks the end of a sequence in the stream.
//
// See https://git-scm.com/docs/protocol-common#_pkt_line_format
// for the official documentation, although it fails to mention the 0001 packets.
type pktLineReader struct {
	b    *bufio.Reader
	size [4]byte
}

// newPktLineReader returns a new pktLineReader reading from r.
func newPktLineReader(r io.Reader) *pktLineReader {
	return &pktLineReader{b: bufio.NewReader(r)}
}

// Next returns the payload of the next packet from the stream.
// If the next packet is a flush packet (length 0000), Next returns nil, io.EOF.
// If the next packet is a delimiter packet (length 0001), Next returns nil, nil.
// If the data stream has ended, Next returns nil, io.ErrUnexpectedEOF.
func (r *pktLineReader) Next() ([]byte, error) {
	_, err := io.ReadFull(r.b, r.size[:])
	if err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	n, err := strconv.ParseUint(string(r.size[:]), 16, 0)
	if err != nil || n == 2 || n == 3 {
		return nil, fmt.Errorf("malformed pkt-line")
	}
	if n == 1 {
		return nil, nil // delimiter
	}
	if n == 0 {
		return nil, io.EOF
	}
	buf := make([]byte, n-4)
	_, err = io.ReadFull(r.b, buf)
	if err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return buf, nil
}

// Lines reads packets from r until a flush packet.
// It returns a string for each packet, with any trailing newline trimmed.
func (r *pktLineReader) Lines() ([]string, error) {
	var lines []string
	for {
		line, err := r.Next()
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			return lines, err
		}
		lines = append(lines, strings.TrimSuffix(string(line), "\n"))
	}
}

// A pktLineWriter writes Git pkt-line-formatted packets.
// See pktLineReader for a description of the packet format.
type pktLineWriter struct {
	b    *bufio.Writer
	size [4]byte
}

// newPktLineWriter returns a new pktLineWriter writing to w.
func newPktLineWriter(w io.Writer) *pktLineWriter {
	return &pktLineWriter{b: bufio.NewWriter(w)}
}

// writeSize writes a four-digit hexadecimal length packet for n.
// Typically n is len(data)+4.
func (w *pktLineWriter) writeSize(n int) {
	hex := "0123456789abcdef"
	w.size[0] = hex[n>>12]
	w.size[1] = hex[(n>>8)&0xf]
	w.size[2] = hex[(n>>4)&0xf]
	w.size[3] = hex[(n>>0)&0xf]
	w.b.Write(w.size[:])
}

// Write writes b as a single packet.
func (w *pktLineWriter) Write(b []byte) (int, error) {
	n := len(b)
	if n+4 > 0xffff {
		return 0, fmt.Errorf("write too large")
	}
	w.writeSize(n + 4)
	w.b.Write(b)
	return n, nil
}

// WriteString writes s as a single packet.
func (w *pktLineWriter) WriteString(s string) (int, error) {
	n := len(s)
	if n+4 > 0xffff {
		return 0, fmt.Errorf("write too large")
	}
	w.writeSize(n + 4)
	w.b.WriteString(s)
	return n, nil
}

// Close writes a terminating flush packet
// and flushes buffered data to the underlying writer.
func (w *pktLineWriter) Close() error {
	w.b.WriteString("0000")
	w.b.Flush()
	return nil
}

// Delim writes a delimiter packet.
func (w *pktLineWriter) Delim() {
	w.b.WriteString("0001")
}