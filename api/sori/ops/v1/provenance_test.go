package opsv1_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
)

// acceptedProto is the accepted D01.4 contract (r3 blocks + r4 comment-only
// patch; unchanged through r7). The values are copied from the central record,
// not from api/provenance.json, so a regenerated provenance cannot silently
// move the contract.
var acceptedProto = map[string]string{
	"sori/ops/v1/common.proto":    "7f844f6d0310ffd13ed46f8cf265b0fd38823d0e86497a9fd1d601ae1000e6d8",
	"sori/ops/v1/discovery.proto": "3ea91f7ab5336a41a8636ec3aacdf090f38fb8dea9b0810b6ea3a12e13ee43be",
	"sori/ops/v1/job.proto":       "14dda4f0d7cc32a32ee90617fb9a578f6c522ea714c2cd4fd2f51b7f8877578f",
	"sori/ops/v1/transfer.proto":  "6f2c4f6156a7ce51e854738ce677b30682f209219f5eae6a9778c562c75af446",
}

const acceptedSetDigest = "5c91a0ecdf941b445fce5b21b9ac601142d0bd7fc1663098f134e5501217f1ca"

// Pinned generator versions (D01.4-r2 §1, CAD-SORI-D01.4-r2 local plugins);
// they must match scripts/proto-gen.sh.
const pinnedBuf = "1.73.0"

var pinnedPlugins = map[string]string{
	"protoc-gen-go":      "v1.36.11",
	"protoc-gen-go-grpc": "1.6.1",
}

const provenanceFile = "sori/ops/v1/GENERATED.provenance"

type plugin struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Opt     string `json:"opt"`
}

type provenance struct {
	SchemaVersion  int               `json:"schemaVersion"`
	ProtoSetDigest string            `json:"protoSetDigest"`
	SourceRevision string            `json:"sourceRevision"`
	BufVersion     string            `json:"bufVersion"`
	Plugins        []plugin          `json:"plugins"`
	Proto          map[string]string `json:"proto"`
	Generated      map[string]string `json:"generated"`
}

func fileDigest(fsys fs.FS, name string) (string, error) {
	b, err := fs.ReadFile(fsys, name)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// setDigest is sha256 over sha256sum lines "<hex>  <path>\n" sorted by path.
func setDigest(files map[string]string) string {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		fmt.Fprintf(&b, "%s  %s\n", files[n], n)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// listed returns every regular file under dir whose name matches one of the
// suffixes, so an unexpected .proto or generated file is not overlooked.
func listed(fsys fs.FS, dir string, suffixes ...string) ([]string, error) {
	var out []string
	err := fs.WalkDir(fsys, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		for _, s := range suffixes {
			if strings.HasSuffix(p, s) {
				out = append(out, p)
				break
			}
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}

func compareSet(kind string, fsys fs.FS, want map[string]string, suffixes ...string) error {
	got, err := listed(fsys, "sori", suffixes...)
	if err != nil {
		return err
	}
	if len(got) != len(want) {
		return fmt.Errorf("%s: tree has %d files %v, provenance lists %d", kind, len(got), got, len(want))
	}
	for _, name := range got {
		w, ok := want[name]
		if !ok {
			return fmt.Errorf("%s: %s is not listed in provenance", kind, name)
		}
		d, err := fileDigest(fsys, name)
		if err != nil {
			return err
		}
		if d != w {
			return fmt.Errorf("%s: %s sha256 %s, provenance %s", kind, name, d, w)
		}
	}
	return nil
}

// verifyTree checks the api/ tree against the accepted contract and the
// recorded provenance. It returns the first mismatch.
func verifyTree(fsys fs.FS) error {
	raw, err := fs.ReadFile(fsys, provenanceFile)
	if err != nil {
		return err
	}
	var p provenance
	if err := json.Unmarshal(raw, &p); err != nil {
		return fmt.Errorf("%s: %w", provenanceFile, err)
	}
	if p.SchemaVersion != 1 || p.SourceRevision == "" {
		return fmt.Errorf("schemaVersion %d sourceRevision %q", p.SchemaVersion, p.SourceRevision)
	}
	if len(p.Proto) != len(acceptedProto) {
		return fmt.Errorf("provenance lists %d proto files, accepted contract has %d", len(p.Proto), len(acceptedProto))
	}
	for name, want := range acceptedProto {
		if p.Proto[name] != want {
			return fmt.Errorf("provenance %s = %q, accepted %s", name, p.Proto[name], want)
		}
	}
	if got := setDigest(p.Proto); got != acceptedSetDigest || p.ProtoSetDigest != acceptedSetDigest {
		return fmt.Errorf("proto_set_digest computed %s recorded %s, accepted %s", got, p.ProtoSetDigest, acceptedSetDigest)
	}
	if p.BufVersion != pinnedBuf {
		return fmt.Errorf("bufVersion %q, pinned %s", p.BufVersion, pinnedBuf)
	}
	if len(p.Plugins) != len(pinnedPlugins) {
		return fmt.Errorf("plugins %v, pinned %v", p.Plugins, pinnedPlugins)
	}
	for _, pl := range p.Plugins {
		if want, ok := pinnedPlugins[pl.Name]; !ok || pl.Version != want || pl.Opt != "paths=source_relative" {
			return fmt.Errorf("plugin %+v, pinned %v", pl, pinnedPlugins)
		}
	}
	if err := compareSet("proto", fsys, p.Proto, ".proto"); err != nil {
		return err
	}
	if len(p.Generated) == 0 {
		return fmt.Errorf("provenance lists no generated files")
	}
	return compareSet("generated", fsys, p.Generated, ".pb.go")
}

func apiTree(t *testing.T) fstest.MapFS {
	t.Helper()
	root := os.DirFS("../../..")
	m := fstest.MapFS{}
	err := fs.WalkDir(root, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(root, p)
		if err != nil {
			return err
		}
		m[p] = &fstest.MapFile{Data: b}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestTreeMatchesAcceptedContractAndProvenance(t *testing.T) {
	if err := verifyTree(apiTree(t)); err != nil {
		t.Fatal(err)
	}
}

// replaceIn edits the first occurrence of old in GENERATED.provenance.
func replaceIn(old, repl string) func(fstest.MapFS) {
	return func(m fstest.MapFS) {
		s := string(m[provenanceFile].Data)
		if !strings.Contains(s, old) {
			panic("provenance does not contain " + old)
		}
		m[provenanceFile] = &fstest.MapFile{Data: []byte(strings.Replace(s, old, repl, 1))}
	}
}

func TestVerifyTreeRejectsMismatch(t *testing.T) {
	const job = "sori/ops/v1/job.proto"
	const gen = "sori/ops/v1/job.pb.go"
	cases := map[string]func(fstest.MapFS){
		"proto byte changed": func(m fstest.MapFS) {
			m[job] = &fstest.MapFile{Data: append([]byte("// x\n"), m[job].Data...)}
		},
		"generated byte changed": func(m fstest.MapFS) {
			m[gen] = &fstest.MapFile{Data: append([]byte("// x\n"), m[gen].Data...)}
		},
		"unlisted generated file": func(m fstest.MapFS) {
			m["sori/ops/v1/extra.pb.go"] = &fstest.MapFile{Data: []byte("package opsv1\n")}
		},
		"unlisted proto file": func(m fstest.MapFS) {
			m["sori/ops/v1/extra.proto"] = &fstest.MapFile{Data: []byte("syntax = \"proto3\";\n")}
		},
		"generated file missing":        func(m fstest.MapFS) { delete(m, gen) },
		"provenance proto entry moved":  replaceIn(acceptedProto[job], strings.Repeat("0", 64)),
		"provenance set digest changed": replaceIn(acceptedSetDigest, strings.Repeat("0", 64)),
		"provenance buf changed":        replaceIn(`"bufVersion": "1.73.0"`, `"bufVersion": "1.57.0"`),
		"provenance plugin changed":     replaceIn(`"version": "v1.36.11"`, `"version": "v1.36.10"`),
		"provenance schema changed":     replaceIn(`"schemaVersion": 1`, `"schemaVersion": 2`),
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m := apiTree(t)
			mutate(m)
			if err := verifyTree(m); err == nil {
				t.Fatal("verifyTree accepted a mismatched tree")
			}
		})
	}
	if _, ok := apiTree(t)[path.Join("sori/ops/v1", "job_grpc.pb.go")]; !ok {
		t.Fatal("job_grpc.pb.go not found in the api tree")
	}
}
