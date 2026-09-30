package deploy

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json/v2"
	"errors"
	"io"
	"strings"
	"testing"
)

type entry struct {
	name, body, link string
	typ              byte
}

// backup builds a .tar.gz: the manifest (unless nil) then the entries.
func backup(t *testing.T, m *BackupManifest, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if m != nil {
		b, _ := json.Marshal(m)
		tw.WriteHeader(&tar.Header{Name: ManifestName, Mode: 0o644, Size: int64(len(b)), Typeflag: tar.TypeReg})
		tw.Write(b)
	}
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		h := &tar.Header{Name: e.name, Mode: 0o600, Typeflag: typ, Linkname: e.link, Uid: 999, Gid: 999}
		if typ == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(e.body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

var testManifest = BackupManifest{Format: 1, Project: "shop", Snapshot: 3, Reason: "manual", Volumes: []BackupVolume{
	{Kind: "volume", Name: "vops-shop-db", Dir: "0-vops-shop-db"},
	{Kind: "bind", Name: "shop/files", Dir: "1-shop_files"},
}}

// read runs readBackup and returns, per volume name, the entries it would extract ("name:body", "name->link").
func read(t *testing.T, data []byte, allowed ...string) (map[string][]string, error) {
	t.Helper()
	got := map[string][]string{}
	check := func(m BackupManifest) error {
		for _, v := range m.Volumes {
			if len(allowed) > 0 && !strings.Contains(strings.Join(allowed, ","), v.Name) {
				return errors.New("unknown volume " + v.Name)
			}
		}
		return nil
	}
	open := func(v BackupVolume) (io.WriteCloser, func() error, error) {
		pr, pw := io.Pipe()
		done := make(chan error, 1)
		go func() {
			tr := tar.NewReader(pr)
			var err error
			for {
				var h *tar.Header
				if h, err = tr.Next(); err != nil {
					break
				}
				b, _ := io.ReadAll(tr)
				s := h.Name + ":" + string(b)
				if h.Linkname != "" {
					s = h.Name + "->" + h.Linkname
				}
				got[v.Name] = append(got[v.Name], s)
			}
			io.Copy(io.Discard, pr)
			if err == io.EOF {
				err = nil
			}
			done <- err
		}()
		if got[v.Name] == nil {
			got[v.Name] = []string{}
		}
		return pw, func() error { return <-done }, nil
	}
	_, err := readBackup(bytes.NewReader(data), check, open)
	return got, err
}

func TestReadBackup(t *testing.T) {
	m := testManifest
	got, err := read(t, backup(t, &m,
		entry{name: "0-vops-shop-db/", typ: tar.TypeDir},
		entry{name: "0-vops-shop-db/PG_VERSION", body: "16"},
		entry{name: "0-vops-shop-db/base/", typ: tar.TypeDir},
		entry{name: "0-vops-shop-db/current", typ: tar.TypeSymlink, link: "/etc/passwd"}, // a symlink may point anywhere
		entry{name: "0-vops-shop-db/hard", typ: tar.TypeLink, link: "0-vops-shop-db/PG_VERSION"},
	), "vops-shop-db", "shop/files")
	if err != nil {
		t.Fatal(err)
	}
	want := "./: PG_VERSION:16 base/: current->/etc/passwd hard->PG_VERSION"
	if s := strings.Join(got["vops-shop-db"], " "); s != want {
		t.Fatalf("entries: %q, want %q", s, want)
	}
	if files, ok := got["shop/files"]; !ok || len(files) != 0 {
		t.Fatalf("a volume without entries must still be opened (empty): %v", got)
	}

	for _, c := range []struct {
		name    string
		data    []byte
		want    string
		allowed []string
	}{
		{"missing manifest", backup(t, nil, entry{name: "0-vops-shop-db/a", body: "x"}), "must be its first entry", nil},
		{"manifest not first", backup(t, nil, entry{name: "0-vops-shop-db/a", body: "x"}, entry{name: ManifestName, body: "{}"}), "must be its first entry", nil},
		{"not gzip", []byte("hello"), "not a .tar.gz", nil},
		{"bad json", backup(t, nil, entry{name: ManifestName, body: "{"}), ManifestName, nil},
		{"unknown volume", backup(t, &m), "unknown volume", []string{"vops-shop-db"}},
		{"traversal", backup(t, &m, entry{name: "0-vops-shop-db/../../etc/passwd", body: "x"}), "..", nil},
		{"traversal in the middle", backup(t, &m, entry{name: "0-vops-shop-db/a/../../1-shop_files/x", body: "x"}), "..", nil},
		{"absolute", backup(t, &m, entry{name: "/etc/passwd", body: "x"}), "absolute", nil},
		{"unknown dir", backup(t, &m, entry{name: "2-other/x", body: "x"}), "not in a volume", nil},
		{"device", backup(t, &m, entry{name: "0-vops-shop-db/sda", typ: tar.TypeBlock}), "device files", nil},
		{"char device", backup(t, &m, entry{name: "0-vops-shop-db/null", typ: tar.TypeChar}), "device files", nil},
		{"fifo", backup(t, &m, entry{name: "0-vops-shop-db/p", typ: tar.TypeFifo}), "fifos", nil},
		{"hard link out of its volume", backup(t, &m, entry{name: "0-vops-shop-db/h", typ: tar.TypeLink, link: "1-shop_files/x"}), "leaves its volume", nil},
		{"hard link absolute", backup(t, &m, entry{name: "0-vops-shop-db/h", typ: tar.TypeLink, link: "/etc/shadow"}), "leaves its volume", nil},
		{"hard link traversal", backup(t, &m, entry{name: "0-vops-shop-db/h", typ: tar.TypeLink, link: "0-vops-shop-db/../../x"}), "leaves its volume", nil},
		{"through a symlink", backup(t, &m, entry{name: "0-vops-shop-db/l", typ: tar.TypeSymlink, link: "/etc"}, entry{name: "0-vops-shop-db/l/cron.d/x", body: "x"}), "symlink", nil},
		{"replacing a symlink", backup(t, &m, entry{name: "0-vops-shop-db/l", typ: tar.TypeSymlink, link: "/etc"}, entry{name: "0-vops-shop-db/l", typ: tar.TypeDir}), "symlink", nil},
		{"not grouped", backup(t, &m, entry{name: "0-vops-shop-db/a", body: "x"}, entry{name: "1-shop_files/b", body: "x"}, entry{name: "0-vops-shop-db/c", body: "x"}), "not together", nil},
	} {
		if _, err := read(t, c.data, c.allowed...); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want an error with %q", c.name, err, c.want)
		}
	}

	bad := testManifest
	bad.Volumes = []BackupVolume{{Kind: "volume", Name: "x", Dir: "../x"}}
	if _, err := read(t, backup(t, &bad)); err == nil || !strings.Contains(err.Error(), "bad volume") {
		t.Errorf("manifest dir with ..: %v", err)
	}
	bad.Volumes = []BackupVolume{{Kind: "device", Name: "x", Dir: "0-x"}}
	if _, err := read(t, backup(t, &bad)); err == nil {
		t.Error("manifest with an unknown kind")
	}
	bad.Volumes, bad.Format = testManifest.Volumes, 2
	if _, err := read(t, backup(t, &bad)); err == nil || !strings.Contains(err.Error(), "format 2") {
		t.Errorf("future format: %v", err)
	}
}
