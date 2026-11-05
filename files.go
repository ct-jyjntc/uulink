package main

import (
	"bytes"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type receiveFile struct {
	rel, final, temp string
	size, mtime      uint64
	handle           *os.File
	written          uint64
	block            uint64
	done, skip       bool
}
type receiveTask struct {
	files    []*receiveFile
	strategy uint64
}
type ftpAckKey struct{ kind, task, index, block uint64 }

type fileService struct {
	opMu    sync.Mutex
	peer    *protocolPeer
	mu      sync.Mutex
	tasks   map[uint64]*receiveTask
	pending map[ftpAckKey]chan []byte
	next    uint64
}

func newFileService(p *protocolPeer) *fileService {
	return &fileService{peer: p, tasks: map[uint64]*receiveTask{}, pending: map[ftpAckKey]chan []byte{}, next: 1000000}
}
func (fs *fileService) close() {
	fs.opMu.Lock()
	defer fs.opMu.Unlock()
	fs.mu.Lock()
	defer fs.mu.Unlock()
	for _, t := range fs.tasks {
		for _, f := range t.files {
			if f.handle != nil {
				f.handle.Close()
			}
			if !f.done && f.temp != "" {
				os.Remove(f.temp)
			}
		}
	}
	fs.tasks = map[uint64]*receiveTask{}
}
func fileError(err error) uint64 {
	if err == nil {
		return 1
	}
	if errors.Is(err, os.ErrNotExist) {
		return 9
	}
	if errors.Is(err, os.ErrPermission) {
		return 4
	}
	return 6
}
func taskID(data []byte) uint64                  { return pbNumber(pbData(data, 1), 1) }
func fileIndex(data []byte) uint64               { return pbNumber(pbData(data, 1), 2) }
func ftpPayload(kind uint64, data []byte) []byte { return putBytes(nil, kind, data) }
func (fs *fileService) reply(channel string, header []byte, kind uint64, data []byte) {
	if err := fs.peer.rpcReply("FILE_DATA_CHANNEL", header, 4, ftpPayload(kind, data)); err != nil {
		log.Printf("FTP reply: %v", err)
	}
}
func ackKey(kind uint64, data []byte) ftpAckKey {
	k := ftpAckKey{kind: kind, task: taskID(data), index: fileIndex(data)}
	if kind == 4 {
		k.block = pbNumber(data, 2)
	}
	return k
}
func (fs *fileService) response(data []byte) {
	body := pbData(data, 4)
	for kind, part := range pbFields(body) {
		key := ackKey(kind, part.data)
		fs.mu.Lock()
		ch := fs.pending[key]
		fs.mu.Unlock()
		if ch != nil {
			select {
			case ch <- body:
			default:
			}
		}
	}
}
func (fs *fileService) call(ctx context.Context, requestID, kind uint64, data []byte) ([]byte, error) {
	fs.mu.Lock()
	fs.next++
	id := requestID
	if id == 0 {
		id = fs.next
	}
	ch := make(chan []byte, 1)
	responseKind := map[uint64]uint64{3: 3, 4: 4, 9: 6}[kind]
	key := ackKey(responseKind, data)
	fs.pending[key] = ch
	fs.mu.Unlock()
	defer func() { fs.mu.Lock(); delete(fs.pending, key); fs.mu.Unlock() }()
	request := putBytes(nil, 1, putVar(nil, 1, id))
	request = putBytes(request, 8, ftpPayload(kind, data))
	channel := "FILE_DATA_CHANNEL"
	if err := fs.peer.send(channel, 21, request); err != nil {
		return nil, err
	}
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, fmt.Errorf("file transfer acknowledgement timeout")
	case b := <-ch:
		return b, nil
	}
}
func normalizedPath(path string) string {
	path = strings.ReplaceAll(path, `\`, "/")
	if path == "" || path == "~" || path == ":/" {
		return "/"
	}
	if !filepath.IsAbs(path) {
		path = "/" + path
	}
	return filepath.Clean(path)
}
func safeRelative(root, rel string) (string, error) {
	rel = strings.ReplaceAll(rel, `\`, "/")
	if rel == "" || filepath.IsAbs(rel) {
		return "", fmt.Errorf("invalid relative path")
	}
	path := filepath.Clean(rel)
	if path == ".." || strings.HasPrefix(path, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("path escapes destination")
	}
	return filepath.Join(root, path), nil
}
func fileInfo(rel string, info os.FileInfo) []byte {
	b := putBytes(nil, 1, []byte(rel))
	b = putVar(b, 2, uint64(info.Size()))
	return putVar(b, 3, uint64(info.ModTime().Unix()))
}
func (fs *fileService) handle(channel string, header, raw []byte) {
	if !fs.peer.features.files {
		// Never run filesystem operations for an unselected component.
		for kind, v := range pbFields(raw) {
			var reply []byte
			responseKind := uint64(5)
			switch kind {
			case 10:
				responseKind = 7
				reply = putBytes(nil, 1, pbData(v.data, 1))
				reply = putBytes(reply, 2, pbData(v.data, 2))
				reply = putVar(reply, 4, 4)
			case 1:
				responseKind = 1
				reply = putBytes(nil, 1, pbData(v.data, 1))
				reply = putVar(reply, 3, 4)
			case 2:
				responseKind = 2
				reply = putBytes(nil, 1, pbData(v.data, 1))
				reply = putVar(reply, 2, 4)
			default:
				reply = putVar(nil, 1, 4)
				reply = putBytes(reply, 2, []byte("File component is disabled"))
			}
			fs.reply(channel, header, responseKind, reply)
		}
		return
	}
	fs.opMu.Lock()
	defer fs.opMu.Unlock()
	for kind, v := range pbFields(raw) {
		d := v.data
		if kind != 4 || os.Getenv("UU_TRACE_FRAMES") == "1" {
			log.Printf("FTP request type=%d bytes=%d", kind, len(d))
		}
		switch kind {
		case 10:
			path := normalizedPath(string(pbData(d, 2)))
			entries, err := os.ReadDir(path)
			out := putBytes(nil, 1, pbData(d, 1))
			out = putBytes(out, 2, pbData(d, 2))
			out = putVar(out, 4, fileError(err))
			for _, entry := range entries {
				info, e := entry.Info()
				if e != nil {
					continue
				}
				t := uint64(4)
				if entry.IsDir() {
					t = 0
				}
				if info.Mode()&os.ModeSymlink != 0 {
					t = 5
					if linked, e := os.Stat(filepath.Join(path, entry.Name())); e == nil && linked.IsDir() {
						t = 2
					}
				}
				b := putVar(nil, 1, t)
				b = putBytes(b, 2, []byte(entry.Name()))
				b = putVar(b, 3, uint64(info.Size()))
				b = putVar(b, 4, uint64(info.ModTime().Unix()))
				b = putBytes(b, 5, []byte(filepath.Join(path, entry.Name())))
				out = putBytes(out, 3, b)
			}
			fs.reply(channel, header, 7, out)
		case 5, 6, 7, 8:
			path := normalizedPath(string(pbData(d, 1)))
			if kind == 8 {
				path = normalizedPath(string(pbData(d, 2)))
			}
			var err error
			if path == "" || filepath.Clean(path) == "/" {
				err = fmt.Errorf("cannot operate on filesystem root")
			} else {
				switch kind {
				case 5:
					name := string(pbData(d, 2))
					if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
						err = fmt.Errorf("invalid filename")
					} else {
						err = os.Rename(path, filepath.Join(filepath.Dir(path), name))
					}
				case 6:
					err = os.Mkdir(path, 0755)
				case 7:
					err = os.Remove(path)
				case 8:
					if pbNumber(d, 3) != 0 {
						err = os.RemoveAll(path)
					} else {
						err = os.Remove(path)
					}
				}
			}
			out := putVar(nil, 1, fileError(err))
			if err != nil {
				out = putBytes(out, 2, []byte(err.Error()))
			}
			out = putBytes(out, 3, []byte(path))
			fs.reply(channel, header, 5, out)
		case 11:
			path := normalizedPath(string(pbData(d, 1)))
			out := putBytes(nil, 1, []byte(path))
			fields, _ := parsePB(d)
			for _, field := range fields {
				if field.num != 2 {
					continue
				}
				name := string(field.data)
				_, e := os.Lstat(filepath.Join(path, filepath.Base(name)))
				r := putBytes(nil, 1, field.data)
				if e == nil {
					r = putVar(r, 2, 1)
				}
				out = putBytes(out, 2, r)
			}
			fs.reply(channel, header, 8, out)
		case 1:
			fs.receive(channel, header, d)
		case 3, 4, 9:
			fs.receiveBlock(channel, header, kind, d)
		case 2:
			fs.sendFiles(channel, header, d)
		default:
			log.Printf("FTP unsupported operation %d", kind)
		}
	}
}
func (fs *fileService) receive(channel string, header, d []byte) {
	root := normalizedPath(string(pbData(d, 2)))
	folder := string(pbData(d, 5))
	var err error
	if folder != "" {
		root, err = safeRelative(root, folder)
	}
	t := &receiveTask{strategy: pbNumber(d, 4)}
	fields, _ := parsePB(d)
	if compressed := pbData(d, 6); len(compressed) > 0 {
		zr, e := zlib.NewReader(bytes.NewReader(compressed))
		if e != nil {
			err = e
		} else {
			raw, e := io.ReadAll(io.LimitReader(zr, 16<<20))
			zr.Close()
			if e != nil {
				err = e
			} else {
				list, e := parsePB(raw)
				if e != nil {
					err = e
				} else {
					for _, v := range list {
						if v.num == 1 {
							fields = append(fields, pbField{num: 3, wire: 2, data: v.data})
						}
					}
				}
			}
		}
	}
	out := putBytes(nil, 1, pbData(d, 1))
	for _, v := range fields {
		if v.num != 3 {
			continue
		}
		rel := string(pbData(v.data, 1))
		final, e := safeRelative(root, rel)
		if e != nil {
			err = e
			break
		}
		f := &receiveFile{rel: rel, final: final, size: pbNumber(v.data, 2), mtime: pbNumber(v.data, 3)}
		t.files = append(t.files, f)
		out = putBytes(out, 2, v.data)
	}
	if len(t.files) == 0 && err == nil {
		err = fmt.Errorf("empty file list or unsupported compressed metadata")
	}
	out = putVar(out, 3, fileError(err))
	if err == nil {
		fs.mu.Lock()
		fs.tasks[taskID(d)] = t
		fs.mu.Unlock()
	}
	fs.reply(channel, header, 1, out)
}
func (fs *fileService) receiveBlock(channel string, header []byte, kind uint64, d []byte) {
	fs.mu.Lock()
	t := fs.tasks[taskID(d)]
	fs.mu.Unlock()
	if kind == 9 && fileIndex(d) == ^uint64(0) {
		var err error
		if t == nil {
			err = fmt.Errorf("unknown task")
		} else {
			for _, f := range t.files {
				if f.done || f.skip {
					continue
				}
				if pbNumber(d, 2) != 1 {
					if f.handle != nil {
						f.handle.Close()
						f.handle = nil
					}
					if f.temp != "" {
						os.Remove(f.temp)
					}
					err = fmt.Errorf("transfer canceled")
				} else {
					if e := commitReceivedFile(f); e != nil {
						err = e
					}
				}
			}
		}
		out := putBytes(nil, 1, pbData(d, 1))
		out = putVar(out, 2, fileError(err))
		fs.reply(channel, header, 6, out)
		return
	}
	index := fileIndex(d)
	if index > 0 {
		index--
	} else {
		index = ^uint64(0)
	}
	var f *receiveFile
	var err error
	if t == nil || index >= uint64(len(t.files)) {
		err = fmt.Errorf("unknown transfer or file index")
	} else {
		f = t.files[index]
	}
	out := putBytes(nil, 1, pbData(d, 1))
	responseKind := uint64(6)
	if err == nil {
		switch kind {
		case 3:
			responseKind = 3
			if _, e := os.Lstat(f.final); e == nil {
				switch t.strategy {
				case 3:
					f.skip = true
				case 2:
					ext := filepath.Ext(f.final)
					stem := strings.TrimSuffix(f.final, ext)
					for n := 1; n < 10000; n++ {
						candidate := fmt.Sprintf("%s (%d)%s", stem, n, ext)
						if _, e := os.Lstat(candidate); os.IsNotExist(e) {
							f.final = candidate
							break
						}
					}
				case 1:
				default:
					err = fmt.Errorf("destination already exists")
				}
			}
			if f.skip {
				out = putVar(out, 2, 1)
			} else if err == nil {
				err = os.MkdirAll(filepath.Dir(f.final), 0755)
				if err == nil {
					f.handle, err = os.CreateTemp(filepath.Dir(f.final), ".uulink-part-")
					if err == nil {
						f.temp = f.handle.Name()
					}
				}
			}
		case 4:
			responseKind = 4
			block := pbNumber(d, 2)
			data := pbData(d, 3)
			out = putVar(out, 2, block)
			if f.handle == nil {
				err = fmt.Errorf("file not prepared")
			} else if f.written+uint64(len(data)) > f.size {
				err = fmt.Errorf("file exceeds declared size")
			} else {
				n, e := f.handle.Write(data)
				err = e
				f.written += uint64(n)
				f.block = block
				out = putVar(out, 4, uint64(n))
			}
		case 9:
			if f.skip {
				f.done = true
			} else if f.handle == nil || f.written != f.size {
				err = fmt.Errorf("file size mismatch")
			} else {
				err = f.handle.Sync()
				f.handle.Close()
				f.handle = nil
				if err == nil {
					err = os.Chmod(f.temp, 0644)
				}
				if err == nil {
					err = os.Rename(f.temp, f.final)
				}
				if err == nil {
					f.done = true
					stamp := time.UnixMilli(int64(f.mtime))
					if f.mtime < 100000000000 {
						stamp = time.Unix(int64(f.mtime), 0)
					}
					os.Chtimes(f.final, stamp, stamp)
				}
			}
		}
	}
	switch kind {
	case 3:
		responseKind = 3
		out = putVar(out, 3, fileError(err))
	case 4:
		responseKind = 4
		out = putVar(out, 3, fileError(err))
	default:
		out = putVar(out, 2, fileError(err))
		if err != nil {
			out = putBytes(out, 3, []byte(err.Error()))
		}
	}
	if err != nil {
		log.Printf("FTP receive error: %v", err)
	}
	fs.reply(channel, header, responseKind, out)
}
func commitReceivedFile(f *receiveFile) error {
	if f.done || f.skip {
		return nil
	}
	if f.handle == nil || f.written != f.size {
		return fmt.Errorf("file size mismatch: %d/%d", f.written, f.size)
	}
	err := f.handle.Sync()
	f.handle.Close()
	f.handle = nil
	if err == nil {
		err = os.Chmod(f.temp, 0644)
	}
	if err == nil {
		err = os.Rename(f.temp, f.final)
	}
	if err == nil {
		f.done = true
		stamp := time.UnixMilli(int64(f.mtime))
		if f.mtime < 100000000000 {
			stamp = time.Unix(int64(f.mtime), 0)
		}
		os.Chtimes(f.final, stamp, stamp)
	}
	return err
}

// Current official clients use listData even when the legacy files field is present.
// Omitting it leaves their download file array empty and can trigger a bounds trap.
func compressFileList(list []byte) ([]byte, error) {
	var b bytes.Buffer
	zw := zlib.NewWriter(&b)
	if _, err := zw.Write(list); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
func (fs *fileService) sendFiles(channel string, header, d []byte) {
	if len(pbData(d, 2)) == 0 {
		out := putBytes(nil, 1, pbData(d, 1))
		out = putVar(out, 2, 3)
		fs.reply(channel, header, 2, out)
		return
	}
	path := normalizedPath(string(pbData(d, 2)))
	info, err := os.Stat(path)
	type item struct {
		path, rel string
		info      os.FileInfo
	}
	items := []item{}
	folder := ""
	if err == nil {
		if info.IsDir() {
			folder = filepath.Base(path)
			err = filepath.Walk(path, func(p string, inf os.FileInfo, e error) error {
				if e != nil {
					return e
				}
				if inf.Mode().IsRegular() {
					rel, e := filepath.Rel(path, p)
					if e != nil {
						return e
					}
					items = append(items, item{p, rel, inf})
				}
				return nil
			})
		} else if info.Mode().IsRegular() {
			items = append(items, item{path, filepath.Base(path), info})
		} else {
			err = fmt.Errorf("not a regular file")
		}
	}
	// The native downloader indexes the first item on a successful response.
	// Empty directories cannot be represented by this protocol implementation.
	if err == nil && len(items) == 0 {
		err = fmt.Errorf("directory contains no regular files")
	}
	out := putBytes(nil, 1, pbData(d, 1))
	out = putVar(out, 2, fileError(err))
	for _, f := range items {
		out = putBytes(out, 3, fileInfo(f.rel, f.info))
	}
	out = putBytes(out, 4, []byte(folder))
	var list []byte
	for _, f := range items {
		list = putBytes(list, 1, fileInfo(f.rel, f.info))
	}
	compressed, ce := compressFileList(list)
	if ce != nil {
		log.Printf("FTP metadata compression: %v", ce)
		return
	}
	out = putBytes(out, 5, compressed)
	fs.reply(channel, header, 2, out)
	if err != nil {
		return
	}
	task := taskID(d)
	go func() {
		for index, f := range items {
			id := putVar(nil, 1, task)
			id = putVar(id, 2, uint64(index+1))
			ask := putBytes(nil, 1, id)
			ask = putVar(ask, 2, uint64(f.info.ModTime().Unix()))
			ask = putVar(ask, 3, uint64(f.info.Size()))
			ack, e := fs.call(fs.peer.ctx, pbNumber(header, 1), 3, ask)
			if e != nil {
				log.Printf("FTP send prepare: %v", e)
				return
			}
			confirm := pbData(ack, 3)
			if pbNumber(confirm, 3) != 1 {
				log.Printf("FTP peer refused file")
				return
			}
			if pbNumber(confirm, 2) != 0 {
				continue
			}
			file, e := os.Open(f.path)
			if e != nil {
				return
			}
			offset := pbNumber(confirm, 4)
			if offset > uint64(f.info.Size()) {
				file.Close()
				return
			}
			file.Seek(int64(offset), io.SeekStart)
			buf := make([]byte, 32*1024)
			block := uint64(0)
			for {
				n, re := file.Read(buf)
				if n > 0 {
					data := putBytes(nil, 1, id)
					data = putVar(data, 2, block)
					data = putBytes(data, 3, buf[:n])
					ack, e = fs.call(fs.peer.ctx, pbNumber(header, 1), 4, data)
					if e != nil || pbNumber(pbData(ack, 4), 3) != 1 {
						file.Close()
						log.Printf("FTP send block failed: %v", e)
						return
					}
					block++
				}
				if re != nil {
					if re != io.EOF {
						file.Close()
						return
					}
					break
				}
			}
			file.Close()
			complete := putBytes(nil, 1, id)
			complete = putVar(complete, 2, 1)
			if _, e = fs.call(fs.peer.ctx, pbNumber(header, 1), 9, complete); e != nil {
				log.Printf("FTP send complete: %v", e)
				return
			}
		}
		id := putVar(nil, 1, task)
		id = putVar(id, 2, ^uint64(0))
		complete := putBytes(nil, 1, id)
		complete = putVar(complete, 2, 1)
		if _, e := fs.call(fs.peer.ctx, pbNumber(header, 1), 9, complete); e != nil {
			log.Printf("FTP task complete: %v", e)
		}
	}()
}
