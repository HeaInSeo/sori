package opsv1_test

// Golden fixtures of D01.4-r3 §8 (carried unchanged by r4–r7). The encoders
// below are test-only reference implementations of the r3 §1.1 envelope, the
// D01.1–.3-r2 §A.8 netstring and the B.2.2 namespace MAC. They exist to check
// the accepted vectors and are not product code. fixture-proof:v1 is the
// fixture placeholder (proof_digest = whole-file sha256), never the product
// proof algorithm. Only Go runs here; the C# side is not executed.

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"

	opsv1 "github.com/HeaInSeo/sori/api/sori/ops/v1"
)

const envelopeMagic = "SORI-REQUEST-MEANING"

func u32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }
func u64(v uint64) []byte { return binary.BigEndian.AppendUint64(nil, v) }

// fields encodes field records with ids 1..n in order.
func fields(vals ...[]byte) []byte {
	var b []byte
	for i, v := range vals {
		b = append(b, u32(uint32(i+1))...)
		b = append(b, u32(uint32(len(v)))...)
		b = append(b, v...)
	}
	return b
}

func repeated(elems ...[]byte) []byte {
	b := u32(uint32(len(elems)))
	for _, e := range elems {
		b = append(b, u32(uint32(len(e)))...)
		b = append(b, e...)
	}
	return b
}

func envelope(kind uint32, vals ...[]byte) []byte {
	b := append([]byte(envelopeMagic), 0x00)
	b = append(b, u32(1)...)
	b = append(b, u32(kind)...)
	b = append(b, u32(uint32(len(vals)))...)
	return append(b, fields(vals...)...)
}

func sum(b []byte) []byte { s := sha256.Sum256(b); return s[:] }

func netstrings(vals ...string) []byte {
	var b []byte
	for _, v := range vals {
		b = append(b, strconv.Itoa(len(v))+":"+v+","...)
	}
	return b
}

type member struct{ key, role, format, path, content string }

// fixtureMembers is the r3 §8 E001 member set, sorted by semantic_key.
func fixtureMembers(r2role string) []member {
	return []member{
		{"r1", "reads-forward", "fastq.gz", "reads_R1.fastq.gz", "fixture-r1-content"},
		{"r2", r2role, "fastq.gz", "reads_R2.fastq.gz", "fixture-r2-content"},
	}
}

const cardinalitySingle = 1 // CARDINALITY_SINGLE in transfer.proto

func a8(ms []member) []byte {
	vals := []string{"sori-local-import-observed:v1"}
	for _, m := range ms {
		vals = append(vals, m.key, m.role, m.format, "SINGLE", "fixture-proof:v1", hex.EncodeToString(sum([]byte(m.content))))
	}
	return netstrings(vals...)
}

func declarations(ms []member) []byte {
	elems := make([][]byte, 0, len(ms))
	for _, m := range ms {
		elems = append(elems, fields([]byte(m.key), []byte(m.role), []byte(m.format), u32(cardinalitySingle), u32(1)))
	}
	return repeated(elems...)
}

func localImport(intent uint32, value string, ms []member) []byte {
	return envelope(1, []byte("sori-local-import:v1"), u32(intent), []byte(value), sum(a8(ms)), declarations(ms))
}

func beginUpload(ms []member) []byte {
	files := make([][]byte, 0, len(ms))
	for i, m := range ms {
		files = append(files, fields([]byte(m.path), u64(2684354560), sum([]byte(m.content)), u32(uint32(i))))
	}
	return envelope(5, []byte("sori-local-import:v1"), declarations(ms), repeated(files...))
}

var (
	rev  = "rev_" + strings.Repeat("a", 52)
	bind = "bind_" + strings.Repeat("b", 52)
	job  = "job_" + strings.Repeat("c", 52)
)

func m6() []byte { return envelope(4, []byte(job)) }

func TestMeaningFixturesM1toM8(t *testing.T) {
	cases := []struct {
		name string
		env  []byte
		size int
		sha  string
	}{
		{"M1", localImport(1, "run42", fixtureMembers("reads-reverse")), 288, "dd3bf243e6fd45dbf40607da55af68d665f5953e2407701b5745627c7c958da8"},
		{"M2", localImport(1, "run42", fixtureMembers("reads-forward-2")), 290, "da204e88e282736d738ec6de1a0b73aeb9089edf431db62250713cdef6de92fa"},
		{"M3", localImport(2, "run42", fixtureMembers("reads-reverse")), 288, "b64e53dd99996eee62497937305c7b9c9ac22e58bc8cc4575398052bb3c678b6"},
		{"M4", envelope(2, []byte("harbor.lab.local/genomics/run42"), sum([]byte("fixture-manifest"))), 112,
			"2e6273dfe09ec29ed3190d1906f908d8a1c861c93c10b949ca087dd87680d719"},
		{"M5", envelope(3, []byte(rev), []byte(bind)), 162, "1fe887c6fe9d269e7bd2067d52dc8c6b6a988e5bac72d402e344bc3238f80874"},
		{"M6", m6(), 97, "20101b684164c24828066d94cc7e81866a05a5f0c294a77c7c32f18764b1e31f"},
		{"M7", beginUpload(fixtureMembers("reads-reverse")), 429, "ed5185c4fa0618c1271031abc0ebae7ac5277c74a0ff8abefcffcc0811178ed1"},
		{"M8", envelope(6, []byte(rev), []byte(bind)), 162, "de9433b087cf4ea4554902e4976e47ea5187622ef8512494aa58d1d7b865fdfe"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if len(c.env) != c.size || hex.EncodeToString(sum(c.env)) != c.sha {
				t.Fatalf("%d bytes sha256 %x, want %d bytes %s", len(c.env), sum(c.env), c.size, c.sha)
			}
			if err := decodeEnvelope(c.env); err != nil {
				t.Fatalf("decoder rejects the golden envelope: %v", err)
			}
		})
	}
	// M6 header bytes given in full by r3 §8.
	if got := hex.EncodeToString(m6()[:41]); got != "534f52492d524551554553542d4d45414e494e47000000000100000004000000010000000100000038" {
		t.Fatalf("M6 header %s", got)
	}
	// M1' (same content, new transfer locator) is M1: transfer_id is not a field.
	if !bytes.Equal(localImport(1, "run42", fixtureMembers("reads-reverse")), cases[0].env) {
		t.Fatal("M1' differs from M1")
	}
}

func TestA8ObservedChecksum(t *testing.T) {
	b := a8(fixtureMembers("reads-reverse"))
	if len(b) != 293 || hex.EncodeToString(sum(b)) != "f692ea186eaf6f4f210b012d845a0a77f3113705f2b8e4b3a34892bc58e84b77" {
		t.Fatalf("A8 %d bytes sha256 %x", len(b), sum(b))
	}
}

// fieldCount is the r3 §1.3 table size per rpc_kind.
var fieldCount = map[uint32]uint32{1: 5, 2: 2, 3: 2, 4: 1, 5: 3, 6: 2}

// decodeEnvelope applies the r3 §1.1 decoder rule to the top level: magic and
// version, known rpc_kind, field_count equal to the table, ascending unique
// known field ids, lengths inside the input and no trailing bytes. Value-level
// checks (UTF-8 on string fields, fixed widths) apply to M6's job_id here.
func decodeEnvelope(b []byte) error {
	hdr := len(envelopeMagic) + 1 + 12
	if len(b) < hdr || string(b[:len(envelopeMagic)]) != envelopeMagic || b[len(envelopeMagic)] != 0 {
		return errors.New("bad magic")
	}
	p := b[len(envelopeMagic)+1:]
	version, kind, count := binary.BigEndian.Uint32(p), binary.BigEndian.Uint32(p[4:]), binary.BigEndian.Uint32(p[8:])
	want, ok := fieldCount[kind]
	switch {
	case version != 1:
		return fmt.Errorf("version %d", version)
	case !ok:
		return fmt.Errorf("unknown rpc_kind %d", kind)
	case count != want:
		return fmt.Errorf("field_count %d, table has %d", count, want)
	}
	rest := b[hdr:]
	for id := uint32(1); id <= count; id++ {
		if len(rest) < 8 {
			return errors.New("truncated field header")
		}
		fid, n := binary.BigEndian.Uint32(rest), binary.BigEndian.Uint32(rest[4:])
		if fid != id {
			return fmt.Errorf("field id %d at position %d", fid, id)
		}
		if uint64(n) > uint64(len(rest)-8) {
			return errors.New("length past end")
		}
		if kind == 4 && !utf8.Valid(rest[8:8+n]) {
			return errors.New("invalid UTF-8")
		}
		rest = rest[8+n:]
	}
	if len(rest) != 0 {
		return fmt.Errorf("%d trailing bytes", len(rest))
	}
	return nil
}

// shortSHA checks a vector the record gives only as "<8 hex>…<4 hex>".
func shortSHA(b []byte, prefix, suffix string) bool {
	h := hex.EncodeToString(sum(b))
	return strings.HasPrefix(h, prefix) && strings.HasSuffix(h, suffix)
}

func TestDecoderNegativesX1toX4(t *testing.T) {
	base := m6()
	twoCount := bytes.Clone(base)
	binary.BigEndian.PutUint32(twoCount[len(envelopeMagic)+1+8:], 2)
	field1 := base[len(envelopeMagic)+1+12:]
	// X3's exact bytes are not reproducible from the record (one-line
	// description, abbreviated hash). This construction matches the
	// description and is checked for rejection only.
	dup := append(bytes.Clone(twoCount), field1...)
	// X4 = M6's field plus an unknown field 9 with value "x", field_count 2;
	// it reproduces the recorded hash.
	unknown := append(bytes.Clone(twoCount), u32(9)...)
	unknown = append(unknown, u32(1)...)
	unknown = append(unknown, 'x')
	cases := []struct {
		name, prefix, suffix string
		b                    []byte
		hashKnown            bool
	}{
		{"X1 trailing 0x00", "ad390d8c", "5458", append(bytes.Clone(base), 0x00), true},
		{"X2 field_count 2 with one field", "2da73b71", "079a", twoCount, true},
		{"X3 duplicate field id 1", "68d991b7", "40b9", dup, false},
		{"X4 unknown field id 9", "7b132c39", "130b", unknown, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.hashKnown && !shortSHA(c.b, c.prefix, c.suffix) {
				t.Fatalf("construction sha256 %x does not match the recorded %s…%s", sum(c.b), c.prefix, c.suffix)
			}
			if !c.hashKnown && shortSHA(c.b, c.prefix, c.suffix) {
				t.Fatal("X3 now reproduces the recorded hash; mark it known")
			}
			if err := decodeEnvelope(c.b); err == nil {
				t.Fatal("decoder accepted a negative vector")
			}
		})
	}
}

// namespaceToken is B.2.2: mac = HMAC-SHA256(K, netstrings("sori-namespace:v1",
// installation, principal, decimal(epoch), hex(nonce), key_id)); token = "sns1." +
// base64url_nopad(u64be(epoch) ‖ nonce ‖ raw key_id ‖ mac).
func namespaceToken(key []byte, keyID, installation, principal string, epoch uint64, nonce []byte) (mac []byte, token string) {
	h := hmac.New(sha256.New, key)
	h.Write(netstrings("sori-namespace:v1", installation, principal, strconv.FormatUint(epoch, 10), hex.EncodeToString(nonce), keyID))
	mac = h.Sum(nil)
	rawID, err := hex.DecodeString(strings.TrimPrefix(keyID, "mk1_"))
	if err != nil {
		panic(err)
	}
	raw := append(u64(epoch), nonce...)
	raw = append(raw, rawID...)
	raw = append(raw, mac...)
	return mac, "sns1." + base64.RawURLEncoding.EncodeToString(raw)
}

func TestNamespaceTokenNS1NS2(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	nonce := bytes.Repeat([]byte{0xa5}, 32)
	const keyID = "mk1_00112233445566778899aabbccddeeff"
	mac, token := namespaceToken(key, keyID, "inst-01", "user:alice", 3, nonce)
	if hex.EncodeToString(mac) != "58962727c79d333b48887413ece69b1e187f73283037f482a90f7168bf5f0fac" {
		t.Fatalf("NS1 mac %x", mac)
	}
	const want = "sns1.AAAAAAAAAAOlpaWlpaWlpaWlpaWlpaWlpaWlpaWlpaWlpaWlpaWlpQARIjNEVWZ3iJmqu8zd7v9Ylicnx50zO0iIdBPs5pseGH9zKDA39IKpD3Fov18PrA"
	if token != want || len(token) != 123 {
		t.Fatalf("NS1 token %s (%d chars)", token, len(token))
	}
	bob, _ := namespaceToken(key, keyID, "inst-01", "user:bob", 3, nonce)
	h := hex.EncodeToString(bob)
	if !strings.HasPrefix(h, "fc0de197") || !strings.HasSuffix(h, "313e") || hmac.Equal(bob, mac) {
		t.Fatalf("NS2 recomputed mac %s, want fc0de197…313e and != NS1", h)
	}
}

func TestDiscoverGoldenP1(t *testing.T) {
	m := &opsv1.DiscoverResponse{
		InstallationId:             "inst-01",
		ActivationEpoch:            3,
		SupportedContractRevisions: []uint32{1},
		TransferLimits:             &opsv1.TransferLimits{MaxChunkBytes: 8388608, InitialCredit: 1, MaxMessageBytes: 9437184},
		Writable:                   opsv1.Writability_WRITABILITY_RW,
	}
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	const wantHex = "0a07696e73742d303110031a0101220c08808080041001188080c0043001"
	if hex.EncodeToString(b) != wantHex || hex.EncodeToString(sum(b)) != "e68750d18cb54c32b500ae6c35ad95a67ab4b6ed26dfaab2376f6a99c671b17a" {
		t.Fatalf("P1 %x", b)
	}
	var back opsv1.DiscoverResponse
	if err := proto.Unmarshal(b, &back); err != nil || !proto.Equal(&back, m) {
		t.Fatalf("P1 round trip %v %v", &back, err)
	}
}
