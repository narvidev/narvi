package wsbridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/internal/platform"
)

// cutView is a frame as a test reads it back after Fit: the cut string
// found at the path Fit reported, and the frame's `cut`.
type cutView struct {
	text string
	cut  *struct {
		Kept  int `json:"kept"`
		Total int `json:"total"`
	}
}

func readCut(t *testing.T, payload []byte, path []string) cutView {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var frame map[string]any
	if err := dec.Decode(&frame); err != nil {
		t.Fatalf("decode fitted frame: %v", err)
	}
	text, ok := stringAt(frame, path)
	if !ok {
		t.Fatalf("no string at %v in %s", path, payload)
	}
	var withCut struct {
		Cut *struct {
			Kept  int `json:"kept"`
			Total int `json:"total"`
		} `json:"cut"`
	}
	if err := json.Unmarshal(payload, &withCut); err != nil {
		t.Fatalf("decode cut: %v", err)
	}
	return cutView{text: text, cut: withCut.Cut}
}

func mustMarshal(t testing.TB, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func tokenFrame(t testing.TB, text string) []byte {
	return mustMarshal(t, sandboxws.Token{Type: "token", MessageId: "prt_1", SessionId: "s", Gen: 1, Text: text})
}

// assertCutOf checks a fitted frame against the string it was cut from:
// at most bound bytes, the string's kept prefix at a UTF-8 boundary, the
// marker line, and a `cut` naming both counts.
func assertCutOf(t *testing.T, fitted []byte, path []string, original string, bound int) cutView {
	t.Helper()
	if len(fitted) > bound {
		t.Fatalf("fitted frame is %d bytes, over the bound %d", len(fitted), bound)
	}
	got := readCut(t, fitted, path)
	if got.cut == nil {
		t.Fatalf("fitted frame %.200s carries no cut", fitted)
	}
	kept, total := got.cut.Kept, got.cut.Total
	if total != len(original) || kept < 0 || kept >= total {
		t.Fatalf("cut = {%d, %d}, want 0 <= kept < total = %d", kept, total, len(original))
	}
	if !utf8.ValidString(original[:kept]) {
		t.Fatalf("kept %d bytes ends inside a character", kept)
	}
	if want := original[:kept] + fmt.Sprintf("\n[text cut at %d of %d bytes on its way from the sandbox]", kept, total); got.text != want {
		t.Fatalf("cut text = %.120q..., want the kept prefix and then the marker line", got.text)
	}
	return got
}

func TestFit_Token(t *testing.T) {
	t.Parallel()

	ascii := strings.Repeat("plan line\n", 4000) // 40,000 bytes
	multi := strings.Repeat("é€😀a", 5000)        // 2+3+4+1 bytes a group, 50,000 bytes
	html := strings.Repeat("<a&b>", 8000)        // each byte but 'a' and 'b' escapes to 6

	t.Run("a frame of exactly the bound is written unchanged", func(t *testing.T) {
		t.Parallel()
		payload := tokenFrame(t, ascii)
		got, path, ok := Fit(payload, len(payload), nil)
		if !ok || !bytes.Equal(got, payload) || path != nil {
			t.Fatalf("Fit at len == bound = %d bytes, %v, %v; want the frame unchanged", len(got), path, ok)
		}
	})

	t.Run("a frame of exactly the bound is written as it came, never re-encoded", func(t *testing.T) {
		t.Parallel()
		// Keys out of order, as a hand-written frame may have them: a
		// re-encode would sort them, so only the inclusive bound leaves
		// the bytes as they are.
		payload := []byte(`{"type":"token","text":"` + ascii[:1000] + `","messageId":"prt_1","sessionId":"s","gen":1}`)
		got, _, ok := Fit(payload, len(payload), nil)
		if !ok || !bytes.Equal(got, payload) {
			t.Fatalf("Fit at len == bound wrote %.80s..., want the frame byte for byte", got)
		}
	})

	t.Run("one byte over is cut, kept text and marker", func(t *testing.T) {
		t.Parallel()
		payload := tokenFrame(t, ascii)
		bound := len(payload) - 1
		got, path, ok := Fit(payload, bound, nil)
		if !ok || len(path) != 1 || path[0] != "text" {
			t.Fatalf("Fit = %v, %v; want a cut of text", path, ok)
		}
		assertCutOf(t, got, path, ascii, bound)
	})

	t.Run("plain text fills the bound exactly", func(t *testing.T) {
		t.Parallel()
		payload := tokenFrame(t, ascii)
		bound := platform.DefaultFrameReadLimitBytes
		got, path, ok := Fit(payload, bound, nil)
		if !ok {
			t.Fatal("Fit refused a cuttable token")
		}
		assertCutOf(t, got, path, ascii, bound)
		if len(got) != bound {
			t.Fatalf("cut frame is %d bytes, want exactly the bound %d: every kept byte of plain text escapes to one", len(got), bound)
		}
	})

	t.Run("the cut ends at a UTF-8 boundary", func(t *testing.T) {
		t.Parallel()
		for bound := platform.DefaultFrameReadLimitBytes - 12; bound <= platform.DefaultFrameReadLimitBytes; bound++ {
			got, path, ok := Fit(tokenFrame(t, multi), bound, nil)
			if !ok {
				t.Fatalf("bound %d: Fit refused", bound)
			}
			assertCutOf(t, got, path, multi, bound)
		}
	})

	t.Run("kept counts unescaped bytes", func(t *testing.T) {
		t.Parallel()
		got, path, ok := Fit(tokenFrame(t, html), platform.DefaultFrameReadLimitBytes, nil)
		if !ok {
			t.Fatal("Fit refused")
		}
		view := assertCutOf(t, got, path, html, platform.DefaultFrameReadLimitBytes)
		if view.cut.Kept > platform.DefaultFrameReadLimitBytes/4 {
			t.Fatalf("kept %d bytes of text escaping mostly six-fold into %d: kept is counted escaped", view.cut.Kept, platform.DefaultFrameReadLimitBytes)
		}
	})

	t.Run("the same frame and bound give the same bytes", func(t *testing.T) {
		t.Parallel()
		payload := tokenFrame(t, multi)
		first, _, _ := Fit(payload, 20000, nil)
		second, _, _ := Fit(payload, 20000, nil)
		if !bytes.Equal(first, second) {
			t.Fatal("two cuts of one frame to one bound differ")
		}
	})

	t.Run("a re-cut keeps the total of the first cut", func(t *testing.T) {
		t.Parallel()
		big := strings.Repeat("0123456789", 120_000) // 1,200,000 bytes, over MaxEventFrameBytes
		first, path, ok := Fit(tokenFrame(t, big), platform.MaxEventFrameBytes, nil)
		if !ok {
			t.Fatal("Fit refused the enqueue cut")
		}
		firstView := assertCutOf(t, first, path, big, platform.MaxEventFrameBytes)
		second, secondPath, ok := Fit(first, platform.DefaultFrameReadLimitBytes, path)
		if !ok || strings.Join(secondPath, "/") != "text" {
			t.Fatalf("re-cut = %v, %v; want a cut of the same string", secondPath, ok)
		}
		secondView := assertCutOf(t, second, secondPath, big, platform.DefaultFrameReadLimitBytes)
		if secondView.cut.Total != firstView.cut.Total || secondView.cut.Kept >= firstView.cut.Kept {
			t.Fatalf("re-cut = %+v after %+v; want the same total, keeping less", *secondView.cut, *firstView.cut)
		}
		if strings.Count(secondView.text, "[text cut at") != 1 {
			t.Fatal("the re-cut kept the first cut's marker")
		}
	})
}

func TestFit_ToolResult(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("go test output line\n", 2500) // 50,000 bytes
	for _, tc := range []struct {
		name     string
		output   sandboxws.ToolResultOutput
		wantPath string
		original string
	}{
		{name: "a tool's output", output: sandboxws.ToolResultOutput{"output": long}, wantPath: "output/output", original: long},
		{name: "a tool's error", output: sandboxws.ToolResultOutput{"error": long}, wantPath: "output/error", original: long},
		{name: "the longest string anywhere under output", output: sandboxws.ToolResultOutput{
			"a": "short", "b": map[string]any{"lines": []any{"x", long, "y"}}, "c": strings.Repeat("z", 100),
		}, wantPath: "output/b/lines/1", original: long},
		{name: "a tie goes to the first key in order", output: sandboxws.ToolResultOutput{
			"b": strings.Repeat("p", 20_000), "a": strings.Repeat("q", 20_000),
		}, wantPath: "output/a", original: strings.Repeat("q", 20_000)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			payload := mustMarshal(t, sandboxws.ToolResult{Type: "tool_result", MessageId: "msg_1", SessionId: "s", Gen: 1, CallId: "call_1", Output: tc.output})
			got, path, ok := Fit(payload, platform.DefaultFrameReadLimitBytes, nil)
			if !ok || strings.Join(path, "/") != tc.wantPath {
				t.Fatalf("Fit = %v, %v; want a cut of %s", path, ok, tc.wantPath)
			}
			assertCutOf(t, got, path, tc.original, platform.DefaultFrameReadLimitBytes)
			var back sandboxws.ToolResult
			if err := json.Unmarshal(got, &back); err != nil || back.Cut == nil || back.CallId != "call_1" {
				t.Fatalf("the cut frame does not decode as a tool_result with its cut: %v", err)
			}
		})
	}
}

func TestFit_ToolCall(t *testing.T) {
	t.Parallel()

	content := strings.Repeat("func main() {}\n", 3000) // 45,000 bytes
	payload := mustMarshal(t, sandboxws.ToolCall{Type: "tool_call", MessageId: "msg_1", SessionId: "s", Gen: 1, CallId: "call_1", ToolName: "write",
		Input: sandboxws.ToolCallInput{"filePath": "/workspace/main.go", "content": content, "mode": 420}})
	got, path, ok := Fit(payload, platform.DefaultFrameReadLimitBytes, nil)
	if !ok || strings.Join(path, "/") != "input/content" {
		t.Fatalf("Fit = %v, %v; want a cut of input/content", path, ok)
	}
	assertCutOf(t, got, path, content, platform.DefaultFrameReadLimitBytes)
	if !bytes.Contains(got, []byte(`"mode":420`)) || !bytes.Contains(got, []byte(`"filePath":"/workspace/main.go"`)) {
		t.Fatalf("the cut changed the input's other members: %.300s", got)
	}
}

func TestFit_NotWritten(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("x", 40_000)
	for _, tc := range []struct {
		name    string
		payload []byte
		bound   int
	}{
		{name: "a type that is never cut", payload: mustMarshal(t, sandboxws.PushError{Type: "push_error", MessageId: "m", SessionId: "s", Gen: 1, AckId: "push_error:m", Error: long}), bound: platform.DefaultFrameReadLimitBytes},
		{name: "a frame with no string to cut", payload: mustMarshal(t, map[string]any{"type": "tool_call", "messageId": "m", "input": map[string]any{"n": 1}, "pad": long}), bound: platform.DefaultFrameReadLimitBytes},
		{name: "a frame still over once its string is empty", payload: mustMarshal(t, map[string]any{"type": "token", "messageId": "m", "text": long, "other": long}), bound: platform.DefaultFrameReadLimitBytes},
		{name: "not JSON", payload: []byte(long), bound: platform.DefaultFrameReadLimitBytes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got, _, ok := Fit(tc.payload, tc.bound, nil); ok {
				t.Fatalf("Fit = %d bytes, ok; want the frame not written", len(got))
			}
		})
	}
}

// FuzzFit holds every cut to its contract, whatever the text and bound:
// the frame fits, the kept bytes are a prefix of the original ending at a
// UTF-8 boundary, kept and total are unescaped byte counts, and the same
// input gives the same bytes.
func FuzzFit(f *testing.F) {
	f.Add("plain text", 40)
	f.Add("é€😀<>&\u2028\u2029\x00\x1f\"\\\n\t", 7)
	f.Add("\xff\xfe invalid", 3)
	f.Fuzz(func(t *testing.T, unit string, repeat int) {
		if repeat < 1 || repeat > 400 {
			return
		}
		text := strings.Repeat(unit, repeat)
		payload := tokenFrame(t, text)
		// The text as the control plane reads it: decoding replaces
		// invalid UTF-8.
		var decoded sandboxws.Token
		if err := json.Unmarshal(payload, &decoded); err != nil {
			t.Fatal(err)
		}
		for _, bound := range []int{len(payload), len(payload) - 1, len(payload) / 2, 200} {
			got, path, ok := Fit(payload, bound, nil)
			if !ok {
				continue
			}
			if len(got) > bound {
				t.Fatalf("Fit to %d gave %d bytes", bound, len(got))
			}
			if len(payload) <= bound {
				if !bytes.Equal(got, payload) {
					t.Fatal("a frame that fits was changed")
				}
				continue
			}
			if path == nil {
				// Re-encoded whole, it fits: encoding/json writes invalid
				// UTF-8 as a 6-byte escape, and the U+FFFD it decodes to
				// re-encodes in 3.
				var whole sandboxws.Token
				if err := json.Unmarshal(got, &whole); err != nil || whole.Text != decoded.Text || whole.Cut != nil {
					t.Fatalf("Fit wrote %.200s, neither a cut nor the whole text", got)
				}
				continue
			}
			assertCutOf(t, got, path, decoded.Text, bound)
			again, _, _ := Fit(payload, bound, nil)
			if !bytes.Equal(got, again) {
				t.Fatal("Fit is not deterministic")
			}
		}
	})
}

func TestJSONEscapedLen_MatchesEncodingJSON(t *testing.T) {
	t.Parallel()

	for _, s := range []string{
		"", "plain", "\"quoted\" \\ back", "\n\r\t\b\f", "\x00\x01\x1f\x7f", "<script>&amp;</script>",
		"\u2028\u2029", "é€😀", "\xff", "a\xe2\x82", "\xed\xa0\x80", "\ufffd", strings.Repeat("<>&", 100),
	} {
		raw := mustMarshal(t, s)
		if got, want := jsonEscapedLen(s), len(raw)-2; got != want {
			t.Errorf("jsonEscapedLen(%q) = %d, want %d (%s)", s, got, want, raw)
		}
	}
}

func FuzzJSONEscapedLen(f *testing.F) {
	f.Add("é€😀<>&\u2028\x00\xff\"\\\n")
	f.Fuzz(func(t *testing.T, s string) {
		raw, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := jsonEscapedLen(s), len(raw)-2; got != want {
			t.Fatalf("jsonEscapedLen(%q) = %d, want %d", s, got, want)
		}
	})
}

func TestCapCriticalText(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		in   string
	}{
		{name: "short text is kept whole", in: "error: failed to push some refs"},
		{name: "exactly the cap is kept whole", in: strings.Repeat("e", criticalTextMaxBytes)},
		{name: "longer text keeps its head", in: strings.Repeat("remote: rejected\n", 3000)},
		{name: "a multi-byte character at the cut is not split", in: strings.Repeat("€", 3000)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := CapCriticalText(tc.in)
			if len(tc.in) <= criticalTextMaxBytes {
				if got != tc.in {
					t.Fatalf("CapCriticalText changed a text within the cap")
				}
				return
			}
			if len(got) > criticalTextMaxBytes || !strings.HasSuffix(got, criticalTextTruncationMarker) || !utf8.ValidString(got) {
				t.Fatalf("CapCriticalText = %d bytes ending %q; want at most %d, valid, ending in the marker", len(got), got[len(got)-20:], criticalTextMaxBytes)
			}
			if head := strings.TrimSuffix(got, criticalTextTruncationMarker); !strings.HasPrefix(tc.in, head) {
				t.Fatal("CapCriticalText kept something other than the text's head")
			}
		})
	}

	// Six-fold escaped, a capped text still leaves a critical frame under
	// the read limit every control plane has.
	worst := mustMarshal(t, sandboxws.PushError{Type: "push_error", MessageId: strings.Repeat("m", 36), SessionId: strings.Repeat("s", 36), Gen: 1 << 30,
		AckId: "push_error:" + strings.Repeat("m", 36), Error: CapCriticalText(strings.Repeat("<", 100_000))})
	if len(worst) > platform.DefaultFrameReadLimitBytes {
		t.Fatalf("a push_error carrying a capped, fully escaped text is %d bytes, over %d", len(worst), platform.DefaultFrameReadLimitBytes)
	}
}

func TestFrameWriteBound(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		header []string
		want   int
	}{
		{name: "stated", header: []string{"1048576"}, want: 1048576},
		{name: "stated below the default", header: []string{"20000"}, want: 20000},
		{name: "absent", want: platform.DefaultFrameReadLimitBytes},
		{name: "empty", header: []string{""}, want: platform.DefaultFrameReadLimitBytes},
		{name: "not a number", header: []string{"1 MiB"}, want: platform.DefaultFrameReadLimitBytes},
		{name: "zero", header: []string{"0"}, want: platform.DefaultFrameReadLimitBytes},
		{name: "negative", header: []string{"-1"}, want: platform.DefaultFrameReadLimitBytes},
		{name: "overflowing", header: []string{"99999999999999999999999"}, want: platform.DefaultFrameReadLimitBytes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resp := &http.Response{Header: http.Header{}}
			for _, v := range tc.header {
				resp.Header.Add(platform.MaxFrameBytesHeader, v)
			}
			if got := frameWriteBound(resp); got != tc.want {
				t.Fatalf("frameWriteBound(%v) = %d, want %d", tc.header, got, tc.want)
			}
		})
	}
	if got := frameWriteBound(nil); got != platform.DefaultFrameReadLimitBytes {
		t.Fatalf("frameWriteBound(nil) = %d, want the default", got)
	}
}
