package admininit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
)

// Reference vectors of SORI-D01-W021B-FMT-PREP-r2 §10 (vectors_r2.py sha256
// 0202a059aaa5780aff808d0eafa5295845e9c599538c486931c26b33888c5339). Every
// byte string below is pinned by its length and sha256 from that table.

const (
	vecI    = "00112233445566778899aabbccddeeff"
	vecTime = "2026-10-26T00:00:00Z"
	vecKID  = "mk1_00112233445566778899aabbccddeeff"
)

var (
	vecV1 = `{"credentials":[],"grants":[],"installation_id":"00112233445566778899aabbccddeeff","schema_version":1}`
	vecV2 = `{"current":{"digest":"sha256:7df26522a43b68b5dfd4e058e736506e5c020174dec4a456377e72a190307fb4","document_b64url":"eyJjcmVkZW50aWFscyI6W10sImdyYW50cyI6W10sImluc3RhbGxhdGlvbl9pZCI6IjAwMTEyMjMzNDQ1NTY2Nzc4ODk5YWFiYmNjZGRlZWZmIiwic2NoZW1hX3ZlcnNpb24iOjF9","owner":"unix-uid:1001","policy_revision":1,"revocation_basis":"none","updated_at":"2026-10-26T00:00:00Z"},"high_water":1,"version":1}` + "\n"
	vecV3 = `{"current_key_id":"mk1_00112233445566778899aabbccddeeff","keys":[{"created_at":"2026-10-26T00:00:00Z","key_id":"mk1_00112233445566778899aabbccddeeff","secret_b64url":"AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8","state":"ACTIVE"}],"version":1}` + "\n"
	vecV4 = `{"activation_epoch":1,"active_writer":{"activation_id":"a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5","host_id":"h1","started_at":"2026-10-26T00:00:00Z"},"cut_counter":0,"installation_id":"00112233445566778899aabbccddeeff","version":1}` + "\n"
	vecV5 = `{"s":"a&b<c>d\"e\\f/` + ue("00e9") + ue("2028") + ue("d83d") + ue("de00") + ue("0001") + ue("001f") + ue("007f") + `\b\f\n\r\t"}`

	// vecV5Value is the string V5 encodes.
	vecV5Value = "a&b<c>d\"e\\f/\u00e9\u2028\U0001F600\x01\x1f\x7f\b\f\n\r\t"
)

// ue returns the six-byte JSON escape backslash, u, h. It keeps those
// escapes out of the source text.
func ue(h string) string { return string(rune(92)) + "u" + h }

func vecTimeValue(t *testing.T) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, vecTime)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func vecKeyring() Keyring {
	secret := make([]byte, 32)
	for i := range secret {
		secret[i] = byte(i)
	}
	return Keyring{CurrentKeyID: vecKID, Keys: []MACKey{{KeyID: vecKID, Secret: secret, State: "ACTIVE", CreatedAt: vecTime}}}
}

func vecEpoch() Epoch {
	return Epoch{
		InstallationID: vecI, ActivationEpoch: 1, CutCounter: 0,
		HostID: "h1", ActivationID: strings.Repeat("a5", 16), StartedAt: vecTime,
	}
}

func checkVector(t *testing.T, name string, got []byte, wantLen int, wantSHA string) {
	t.Helper()
	sum := sha256.Sum256(got)
	if hex.EncodeToString(sum[:]) != wantSHA || (wantLen > 0 && len(got) != wantLen) {
		t.Fatalf("%s: len %d sha256 %x, want len %d sha256 %s\n%q", name, len(got), sum, wantLen, wantSHA, got)
	}
}

// TestTailVectorsPositive: the encoders produce V1-V5 byte for byte and the
// strict readers accept them and return the same values.
func TestTailVectorsPositive(t *testing.T) {
	checkVector(t, "V1 literal", []byte(vecV1), 102, "7df26522a43b68b5dfd4e058e736506e5c020174dec4a456377e72a190307fb4")
	checkVector(t, "V2 literal", []byte(vecV2), 387, "360a812d92ba1c5d482ff2a66d9a1c767d833218c71fb8de9b3086b749294539")
	checkVector(t, "V3 literal", []byte(vecV3), 244, "9469ecee975a066f1ac1259686e531cd0a20deda3a66a2c85feae1e030fd7b69")
	checkVector(t, "V4 literal", []byte(vecV4), 224, "32e68ac26fb95f2cf10b71a1a341717e120cf16c4a1555b23fc1565c161c6110")
	checkVector(t, "V5 literal", []byte(vecV5), 74, "85b50577450f8661c77b6c73725bcaa4696e6d8a5261010f7cac2cc7a1a29a0e")

	doc, err := defaultPolicyDocument(vecI)
	if err != nil || string(doc) != vecV1 {
		t.Fatalf("V1: %q, %v", doc, err)
	}
	if id, err := policyDocumentInstallation(doc); err != nil || id != vecI {
		t.Fatalf("V1 read: %q, %v", id, err)
	}

	ps, err := newDefaultPolicyState(vecI, 1001, vecTimeValue(t))
	if err != nil {
		t.Fatal(err)
	}
	b, err := encodePolicyState(ps)
	if err != nil || string(b) != vecV2 {
		t.Fatalf("V2: %q, %v", b, err)
	}
	got, err := parsePolicyState(b)
	if err != nil || got.Digest != ps.Digest || !bytes.Equal(got.Document, doc) || got.Owner != "unix-uid:1001" ||
		got.PolicyRevision != 1 || got.HighWater != 1 || got.RevocationBasis != "none" || got.UpdatedAt != vecTime {
		t.Fatalf("V2 read: %+v, %v", got, err)
	}

	b, err = encodeKeyring(vecKeyring())
	if err != nil || string(b) != vecV3 {
		t.Fatalf("V3: %q, %v", b, err)
	}
	kr, err := parseKeyring(b)
	if err != nil || kr.CurrentKeyID != vecKID || len(kr.Keys) != 1 || !bytes.Equal(kr.Keys[0].Secret, vecKeyring().Keys[0].Secret) {
		t.Fatalf("V3 read: %+v, %v", kr, err)
	}

	b, err = encodeEpoch(vecEpoch())
	if err != nil || string(b) != vecV4 {
		t.Fatalf("V4: %q, %v", b, err)
	}
	if ep, err := parseEpoch(b); err != nil || ep != vecEpoch() {
		t.Fatalf("V4 read: %+v, %v", ep, err)
	}

	b, err = encodeCanon(map[string]any{"s": vecV5Value})
	if err != nil || string(b) != vecV5 {
		t.Fatalf("V5: %q, %v", b, err)
	}
	v, err := decodeCanon([]byte(vecV5), false)
	if err != nil || v.(map[string]any)["s"] != vecV5Value {
		t.Fatalf("V5 read: %q, %v", v, err)
	}
}

// TestTailVectorsNegative: N1-N15 are built from the positives exactly as
// vectors_r2.py builds them, pinned by sha256, and every one is refused with
// the expected typed failure and cause.
func TestTailVectorsNegative(t *testing.T) {
	v4 := []byte(vecV4)
	rep := func(b []byte, old, new string) []byte {
		t.Helper()
		out := bytes.Replace(b, []byte(old), []byte(new), 1)
		if bytes.Equal(out, b) {
			t.Fatalf("replace %q: no match", old)
		}
		return out
	}
	n4 := "{\n" +
		"  \"activation_epoch\": 1,\n" +
		"  \"active_writer\": {\n" +
		"    \"activation_id\": \"a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5\",\n" +
		"    \"host_id\": \"h1\",\n" +
		"    \"started_at\": \"2026-10-26T00:00:00Z\"\n" +
		"  },\n" +
		"  \"cut_counter\": 0,\n" +
		"  \"installation_id\": \"00112233445566778899aabbccddeeff\",\n" +
		"  \"version\": 1\n" +
		"}\n"
	n5 := `{"version":1,"installation_id":"00112233445566778899aabbccddeeff","cut_counter":0,"activation_epoch":1,"active_writer":{"activation_id":"a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5","host_id":"h1","started_at":"2026-10-26T00:00:00Z"}}` + "\n"
	n13 := `{"s":"a` + ue("0026") + "b" + ue("003c") + "c" + ue("003e") + `d\"e\\f/` + "\u00e9" + ue("2028") + "\U0001F600" + ue("0001") + ue("001f") + "\x7f" + `\b\f\n\r\t"}`
	v2 := []byte(vecV2)
	v3 := []byte(vecV3)
	v5 := []byte(vecV5)

	cases := []struct {
		id     string
		data   []byte
		len    int
		sha    string
		read   func([]byte) error
		kind   TailFailure // 0 for the raw decoder (V5 negatives)
		reason string
	}{
		{"N1", rep(v4, `"cut_counter":0,`, `"cut_counter":0,"cut_counter":0,`), 240, "5953ac3d4c65c8e1e1e64a436183de4fe076a9af0612fb29b2bde551c49236c4", readEpoch, TailNotCanonical, `duplicate key "cut_counter"`},
		{"N2", rep(v4, `"cut_counter":0,`, `"cut_counter":0,"extra":1,`), 234, "a3682390c2fb234d5ec36e8829aa7b9eba3bc76e17633156ad806bf44df1e194", readEpoch, TailInvariant, `unknown key "extra"`},
		{"N3", v4[:len(v4)-1], 223, "5e7f92d8324a28c0270153871b8f2d00874e4043ac5eff1fd97132741f2e11da", readEpoch, TailNotCanonical, "missing trailing newline"},
		{"N4", []byte(n4), 266, "a628fe99a2be4f6fde176a0f839a2a1fc34864109d222aaf9474b9297b523dd5", readEpoch, TailNotCanonical, "bytes differ"},
		{"N5", []byte(n5), 224, "dc77419c69c3d11c73731bff682789b962adc07ec7943d9feda5973164b8b7dd", readEpoch, TailNotCanonical, "bytes differ"},
		{"N6", rep(v4, vecI, strings.ToUpper(vecI)), 224, "8a0724acbe580e85767c4515a37199aea21ee57fcbe8f0f5d5d957d0dc93badd", readEpoch, TailInvariant, "installation_id"},
		{"N7", rep(v4, `"version":1`, `"version":2`), 224, "ca6317b8dfb3e5e6bdbf274eba1143065ef50c358c320aeb82cb69e706532e7f", readEpoch, TailUnknownVersion, "version 2"},
		{"N8", rep(v4, `"activation_epoch":1,`, `"activation_epoch":1.0,`), 226, "399c8f2d235f8b6903d093f615bcb3451763f270fc66068496b4ef122549f668", readEpoch, TailNotCanonical, "fraction or exponent"},
		{"N9", rep(v2, `"high_water":1`, `"high_water":2`), 0, "32fff4fdb72ac075ed4282760298006939825f85e1f71e5e56a94300ca72c6d5", readPolicyState, TailInvariant, "high_water 2 != policy_revision 1"},
		{"N10", rep(v2, "7df26522a43b68b5dfd4e058e736506e5c020174dec4a456377e72a190307fb4", strings.Repeat("0", 64)), 0, "c402a9d512f220a25e86a1887bebe04954d618ce98429ee601b378d1f4c0bbc3", readPolicyState, TailInvariant, "does not match the document"},
		{"N11", rep(v3, "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8", b64url.EncodeToString(vecKeyring().Keys[0].Secret[:31])), 0, "679c32e806e6a7519e654c295626a047178ab56838aa291e4d98e1ce44e42832", readKeyring, TailInvariant, "secret is 31 bytes"},
		{"N12", rep(v3, `"current_key_id":"`+vecKID, `"current_key_id":"mk1_`+strings.Repeat("ff", 16)), 0, "2db8dda7d288d0077c5525104453a6cf74b5efb81220f5756b79ef182a9b2c70", readKeyring, TailInvariant, "does not name an ACTIVE key"},
		{"N13", []byte(n13), 72, "c13ea275264f4f1016bfaf58803253f3a2f66fa6106ed47663640262220da828", readRaw, 0, "non-ASCII byte"},
		{"N14", rep(v5, ue("00e9"), ue("00E9")), 74, "6eee2b1b216b230368515e9bda3c12afa2d7cee4f7f83d64660f0c4da9f24794", readRaw, 0, "bytes differ"},
		{"N15", rep(v5, ue("d83d")+ue("de00"), ue("d83d")), 68, "1aec1c26c5e9cedb702db952941ca6757f84ed6d841da7da25dc99c713f6b507", readRaw, 0, "lone surrogate"},
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			checkVector(t, c.id, c.data, c.len, c.sha)
			err := c.read(c.data)
			if err == nil {
				t.Fatalf("%s accepted", c.id)
			}
			if !strings.Contains(err.Error(), c.reason) {
				t.Fatalf("%s: error %q, want cause %q", c.id, err, c.reason)
			}
			if c.kind == 0 {
				if !errors.Is(err, errNotCanonical) {
					t.Fatalf("%s: error %v does not wrap errNotCanonical", c.id, err)
				}
				return
			}
			var te *TailError
			if !errors.As(err, &te) || te.Kind != c.kind {
				t.Fatalf("%s: error %v, want TailError kind %s", c.id, err, c.kind)
			}
		})
	}
}

func readEpoch(b []byte) error       { _, err := parseEpoch(b); return err }
func readPolicyState(b []byte) error { _, err := parsePolicyState(b); return err }
func readKeyring(b []byte) error     { _, err := parseKeyring(b); return err }
func readRaw(b []byte) error         { _, err := decodeCanon(b, false); return err }

// TestTailB64urlRejectsCRLF: a JSON-escaped CR or LF inside secret_b64url or
// document_b64url is still canonical JSON, and the base64url decoder skips
// it, so the strict readers must refuse it themselves.
func TestTailB64urlRejectsCRLF(t *testing.T) {
	bs := string(rune(92))
	secret := `"secret_b64url":"`
	doc := `"document_b64url":"`
	for _, c := range []struct {
		name   string
		rec    string
		field  string
		read   func([]byte) error
		reason string
	}{
		{"keyring", vecV3, secret, readKeyring, "secret_b64url is not unpadded base64url"},
		{"policy state", vecV2, doc, readPolicyState, "document_b64url is not unpadded base64url"},
	} {
		at := strings.Index(c.rec, c.field)
		if at < 0 {
			t.Fatalf("%s: no %s", c.name, c.field)
		}
		start := at + len(c.field)
		for _, esc := range []string{bs + "n", bs + "r", bs + "r" + bs + "n"} {
			for _, pos := range []struct {
				name string
				off  int
			}{{"leading", 0}, {"inner", 8}} {
				t.Run(c.name+" "+escName(esc)+" "+pos.name, func(t *testing.T) {
					b := []byte(c.rec[:start+pos.off] + esc + c.rec[start+pos.off:])
					if _, err := decodeCanon(b, true); err != nil {
						t.Fatalf("mutated record is not canonical, so it does not test the field: %v", err)
					}
					err := c.read(b)
					if err == nil {
						t.Fatalf("%s with %s accepted", c.name, esc)
					}
					var te *TailError
					if !errors.As(err, &te) || te.Kind != TailInvariant || !strings.Contains(err.Error(), c.reason) {
						t.Fatalf("error %v, want TailError kind %s cause %q", err, TailInvariant, c.reason)
					}
				})
			}
		}
	}
}

// escName names an escape sequence for a subtest without the backslash.
func escName(s string) string { return strings.ReplaceAll(s, string(rune(92)), "bs-") }

// TestCanonParserRefusals covers the value model limits one by one.
func TestCanonParserRefusals(t *testing.T) {
	for _, c := range []struct{ in, reason string }{
		{`{"a":true}`, "value type not allowed"},
		{`{"a":false}`, "value type not allowed"},
		{`{"a":null}`, "value type not allowed"},
		{`{"a":-1}`, "value type not allowed"},
		{`{"a":1e0}`, "fraction or exponent"},
		{`{"a":01}`, "leading zero"},
		{`{"a":9223372036854775808}`, "integer out of range"},
		{`{"a":9223372036854775807}`, ""},
		{"{\"a\":\"x\ty\"}", "raw control byte"},
		{`{"a":"\x"}`, "invalid escape"},
		{`{"a":"` + ue("dc00") + `"}`, "lone surrogate"},
		{`{"a":"` + ue("d83d") + `x"}`, "lone surrogate"},
		{`{"a":"` + ue("d83d") + ue("0041") + `"}`, "lone surrogate"},
		{`{"a":"` + ue("12") + `"}`, "escape"},
		{`{"a":"\/"}`, "bytes differ"},
		{`{"a":1}{}`, "trailing data"},
		{`{"a":1,}`, "object key expected"},
		{`{"b":1,"a":1}`, "bytes differ"},
		{`{"a":[1,2],"b":{}}`, ""},
		{strings.Repeat("[", 18) + strings.Repeat("]", 18), "nesting deeper"},
	} {
		_, err := decodeCanon([]byte(c.in), false)
		switch {
		case c.reason == "" && err != nil:
			t.Errorf("%s: refused: %v", c.in, err)
		case c.reason != "" && (err == nil || !strings.Contains(err.Error(), c.reason)):
			t.Errorf("%s: error %v, want %q", c.in, err, c.reason)
		}
	}
	if _, err := decodeCanon(make([]byte, maxCanonSize+1), false); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Errorf("oversize: %v", err)
	}
}

// TestCanonEncoderRefusals: the encoder never renders a value outside the v1
// model or a string that is not valid UTF-8.
func TestCanonEncoderRefusals(t *testing.T) {
	for _, v := range []any{true, nil, 1, -1, 1.0, uint64(1 << 63), "\xff", map[string]any{"\xff": uint64(1)}} {
		if b, err := encodeCanon(v); err == nil {
			t.Errorf("%#v encoded as %q", v, b)
		}
	}
}

// TestValidateHostID: the §10 grammar cases.
func TestValidateHostID(t *testing.T) {
	for _, ok := range []string{"h1", "node-01.lab", "A~b_c", strings.Repeat("x", 64)} {
		if err := ValidateHostID(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", strings.Repeat("x", 65), "h 1", "h/1", "é", "h1\n"} {
		if err := ValidateHostID(bad); !errors.Is(err, ErrHostIDInvalid) {
			t.Errorf("%q: %v, want ErrHostIDInvalid", bad, err)
		}
	}
	e := vecEpoch()
	e.HostID = "h 1"
	if _, err := encodeEpoch(e); !errors.Is(err, ErrHostIDInvalid) {
		t.Errorf("encodeEpoch with an invalid host_id: %v", err)
	}
}

// TestTailRecordInvariants: field checks that the r2 vectors do not reach.
func TestTailRecordInvariants(t *testing.T) {
	kr := vecKeyring()
	kr.Keys = append(kr.Keys, kr.Keys[0])
	if _, err := encodeKeyring(kr); err == nil || !strings.Contains(err.Error(), "duplicate key_id") {
		t.Errorf("duplicate key_id: %v", err)
	}
	kr = vecKeyring()
	kr.Keys[0].State = "RETIRED"
	if _, err := encodeKeyring(kr); err == nil || !strings.Contains(err.Error(), "ACTIVE") {
		t.Errorf("retired current key: %v", err)
	}
	if _, err := encodeKeyring(Keyring{}); err == nil {
		t.Error("empty keyring encoded")
	}
	// A non-empty grants list is refused until its shape is defined.
	if _, err := policyDocumentInstallation([]byte(`{"credentials":[],"grants":[1],"installation_id":"` + vecI + `","schema_version":1}`)); err == nil {
		t.Error("non-empty grants accepted")
	}
	// An epoch "retired" field is refused in version 1.
	b := bytes.Replace([]byte(vecV4), []byte(`"version":1`), []byte(`"retired":{},"version":1`), 1)
	if _, err := parseEpoch(b); err == nil || !strings.Contains(err.Error(), `unknown key "retired"`) {
		t.Error("epoch with retired accepted")
	}
	for _, o := range []string{"unix-uid:", "unix-uid:01", "unix-uid:-1", "uid:1001", "unix-uid:4294967296"} {
		if validOwner(o) {
			t.Errorf("owner %q accepted", o)
		}
	}
	for _, s := range []string{"2026-10-26T00:00:00+00:00", "2026-10-26T00:00:00.5Z", "2026-13-26T00:00:00Z"} {
		if validTailTime(s) {
			t.Errorf("time %q accepted", s)
		}
	}
}
