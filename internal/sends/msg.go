package sends

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"strings"
	"time"
	"unicode/utf8"
)

// parsed holds the fields extracted from a raw message for display.
type parsed struct {
	subject string
	text    string
	html    string
}

const maxPartBytes = 2 << 20 // per-MIME-part read cap during parsing

// parseMsg extracts the subject and the plain/HTML bodies of a raw message.
// It tolerates both CRLF and LF line endings and common content-transfer
// encodings; unparseable or exotic messages degrade to showing the raw body
// as plain text.
func parseMsg(data []byte) parsed {
	var p parsed
	headEnd, sepLen := headerEnd(data)
	if headEnd < 0 {
		// Not a message with headers: show the whole payload as text.
		p.text = string(decodeCharset("", data))
		return p
	}
	head := data[:headEnd-sepLen]
	body := data[headEnd:]

	hdrs, err := parseHeaders(head)
	if err != nil {
		p.text = string(decodeCharset("", data))
		return p
	}
	if raw := hdrs["subject"]; raw != "" {
		p.subject = decodeHeaderWords(raw)
	}

	ct := hdrs["content-type"]
	mt, params, err := mime.ParseMediaType(ct)
	if err != nil {
		mt = "text/plain"
		params = nil
	}
	cte := hdrs["content-transfer-encoding"]
	charset := ""
	if params != nil {
		charset = params["charset"]
	}

	if strings.HasPrefix(mt, "multipart/") && params != nil && params["boundary"] != "" {
		parts := 0
		var walk func(r io.Reader, b string, depth int)
		walk = func(r io.Reader, b string, depth int) {
			if depth > 4 || parts > 200 {
				return
			}
			mr := multipart.NewReader(r, b)
			for {
				part, err := mr.NextPart()
				if err != nil {
					return
				}
				parts++
				collectPart(part, depth, &p, walk)
				part.Close()
			}
		}
		walk(bytes.NewReader(body), params["boundary"], 0)
	} else {
		collectBody(body, mt, charset, cte, &p)
	}

	// Fallback for minimal messages (headers but an unrecognized body): show
	// the decoded payload instead of an empty preview.
	if !strings.HasPrefix(mt, "multipart/") && p.text == "" && p.html == "" && len(body) > 0 {
		collectBody(body, "text/plain", charset, cte, &p)
	}
	return p
}

// collectPart routes one MIME part: nested multiparts are walked, leaf parts
// contribute their text/html payload.
func collectPart(part *multipart.Part, depth int, p *parsed, walk func(io.Reader, string, int)) {
	ct := part.Header.Get("Content-Type")
	mt, params, err := mime.ParseMediaType(ct)
	if err != nil {
		mt = "text/plain"
		params = nil
	}
	if strings.HasPrefix(mt, "multipart/") && params != nil && params["boundary"] != "" {
		walk(part, params["boundary"], depth+1)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(part, maxPartBytes))
	if err != nil {
		return
	}
	charset := ""
	if params != nil {
		charset = params["charset"]
	}
	collectBody(raw, mt, charset, part.Header.Get("Content-Transfer-Encoding"), p)
}

func collectBody(raw []byte, mediaType, charset, cte string, p *parsed) {
	decoded := decodeCTE(cte, raw)
	decoded = decodeCharset(charset, decoded)
	s := strings.TrimRight(string(decoded), "\r\n")
	switch {
	case mediaType == "text/html":
		p.html = s
	case strings.HasPrefix(mediaType, "text/"):
		p.text = s
	}
}

// headerEnd locates the blank line separating headers from the body. It
// returns the index just after the separator and the separator length.
func headerEnd(data []byte) (int, int) {
	if i := bytes.Index(data, []byte("\r\n\r\n")); i >= 0 {
		return i + 4, 4
	}
	if i := bytes.Index(data, []byte("\n\n")); i >= 0 {
		return i + 2, 2
	}
	return -1, 0
}

func parseHeaders(head []byte) (map[string]string, error) {
	hdrs := map[string]string{}
	lastKey := ""
	for _, ln := range bytes.Split(head, []byte("\n")) {
		ln = bytes.TrimSuffix(ln, []byte("\r"))
		if len(ln) == 0 {
			continue
		}
		if ln[0] == ' ' || ln[0] == '\t' {
			if lastKey != "" {
				hdrs[lastKey] += " " + strings.TrimSpace(string(ln))
			}
			continue
		}
		k, v, ok := bytes.Cut(ln, []byte(":"))
		if !ok {
			continue
		}
		lastKey = strings.ToLower(strings.TrimSpace(string(k)))
		hdrs[lastKey] = strings.TrimSpace(string(v))
	}
	return hdrs, nil
}

func decodeHeaderWords(raw string) string {
	dec := mime.WordDecoder{CharsetReader: charsetReader}
	if s, err := dec.DecodeHeader(raw); err == nil {
		return s
	}
	return raw
}

// charsetReader converts any charset into UTF-8 text so encoded-words decode
// even when the charset is not registered with the mime package.
func charsetReader(charset string, input io.Reader) (io.Reader, error) {
	b, err := io.ReadAll(io.LimitReader(input, 64<<10))
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(decodeCharset(charset, b)), nil
}

func decodeCTE(cte string, raw []byte) []byte {
	switch strings.ToLower(strings.TrimSpace(cte)) {
	case "base64":
		compact := strings.Map(func(r rune) rune {
			if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
				return -1
			}
			return r
		}, string(raw))
		if out, err := base64.StdEncoding.DecodeString(compact); err == nil {
			return out
		}
		return raw
	case "quoted-printable":
		if out, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(raw))); err == nil {
			return out
		}
		return raw
	default:
		return raw
	}
}

// decodeCharset converts a body in a declared charset into UTF-8 bytes.
// Unknown charsets are passed through when already valid UTF-8.
func decodeCharset(charset string, b []byte) []byte {
	cs := strings.ToLower(strings.TrimSpace(charset))
	switch cs {
	case "", "utf-8", "utf8", "us-ascii", "ascii":
		return b
	case "iso-8859-1", "iso8859-1", "latin1":
		return latin1ToUTF8(b)
	case "windows-1252", "cp1252":
		return cp1252ToUTF8(b)
	}
	if utf8.Valid(b) {
		return b
	}
	return latin1ToUTF8(b)
}

func latin1ToUTF8(b []byte) []byte {
	var sb strings.Builder
	sb.Grow(len(b))
	for _, c := range b {
		if c < 0x80 {
			sb.WriteByte(c)
		} else {
			sb.WriteRune(rune(c))
		}
	}
	return []byte(sb.String())
}

var cp1252High = []rune{
	'€', '\u0081', '‚', 'ƒ', '„', '…', '†', '‡', 'ˆ', '‰', 'Š', '‹', 'Œ', '\u008d', 'Ž', '\u008f',
	'\u0090', '‘', '’', '“', '”', '•', '–', '—', '˜', '™', 'š', '›', 'œ', '\u009d', 'ž', 'Ÿ',
}

func cp1252ToUTF8(b []byte) []byte {
	var sb strings.Builder
	sb.Grow(len(b))
	for _, c := range b {
		switch {
		case c < 0x80:
			sb.WriteByte(c)
		case c >= 0xA0:
			sb.WriteRune(rune(c)) // cp1252 is latin1 for 0xA0-0xFF
		default:
			sb.WriteRune(cp1252High[c-0x80])
		}
	}
	return []byte(sb.String())
}

// BuildMessage assembles a ready-to-send RFC 5322 message (CRLF line
// endings) for the compose endpoint. id is embedded in the Message-ID header
// so recipients can correlate answers with the queue. Bodies are encoded
// quoted-printable to survive non-ASCII text and long lines.
func BuildMessage(id, from string, to []string, subject, text, html string) ([]byte, error) {
	if id == "" {
		return nil, fmt.Errorf("id must not be empty")
	}
	if from == "" {
		return nil, fmt.Errorf("sender must not be empty")
	}
	if len(to) == 0 {
		return nil, fmt.Errorf("at least one recipient is required")
	}
	if strings.TrimSpace(text) == "" && strings.TrimSpace(html) == "" {
		return nil, fmt.Errorf("a text or HTML body is required")
	}
	for _, v := range append(append([]string{}, to...), from, subject) {
		if strings.ContainsAny(v, "\r\n") {
			return nil, fmt.Errorf("header values must not contain line breaks")
		}
	}
	if subject == "" {
		subject = "(no subject)"
	}

	var b bytes.Buffer
	w := func(format string, args ...any) {
		fmt.Fprintf(&b, format+"\r\n", args...)
	}
	w("Date: %s", time.Now().Format(time.RFC1123Z))
	w("From: %s", from)
	w("To: %s", strings.Join(to, ", "))
	w("Subject: %s", mime.QEncoding.Encode("utf-8", subject))
	w("Message-ID: <%s@carteiro>", id)
	w("MIME-Version: 1.0")
	w("Auto-Submitted: auto-generated")

	var boundary [8]byte
	if _, err := rand.Read(boundary[:]); err != nil {
		return nil, fmt.Errorf("generating boundary: %w", err)
	}
	bstr := "carteiro-" + hex.EncodeToString(boundary[:])

	writePart := func(mediaType string, content string) {
		w("--%s", bstr)
		w("Content-Type: %s; charset=\"utf-8\"", mediaType)
		w("Content-Transfer-Encoding: quoted-printable")
		w("")
		b.Write(qpEncode([]byte(content)))
		b.WriteString("\r\n")
	}

	switch {
	case html != "" && text != "":
		w("Content-Type: multipart/alternative; boundary=\"%s\"", bstr)
		w("")
		writePart("text/plain", text)
		writePart("text/html", html)
		w("--%s--", bstr)
	case html != "":
		w("Content-Type: text/html; charset=\"utf-8\"")
		w("Content-Transfer-Encoding: quoted-printable")
		w("")
		b.Write(qpEncode([]byte(html)))
		b.WriteString("\r\n")
	default:
		w("Content-Type: text/plain; charset=\"utf-8\"")
		w("Content-Transfer-Encoding: quoted-printable")
		w("")
		b.Write(qpEncode([]byte(text)))
		b.WriteString("\r\n")
	}
	return b.Bytes(), nil
}

// qpEncode encodes bytes as RFC 2045 quoted-printable with CRLF line
// endings, soft-wrapping at 76 characters.
func qpEncode(in []byte) []byte {
	var out bytes.Buffer
	col := 0
	for i := 0; i < len(in); i++ {
		c := in[i]
		if c == '\r' {
			continue
		}
		if c == '\n' {
			out.WriteString("\r\n")
			col = 0
			continue
		}
		var tok string
		switch {
		case c == '=':
			tok = "=3D"
		case c == ' ' || c == '\t':
			if i+1 < len(in) && (in[i+1] == '\n' || in[i+1] == '\r') {
				tok = fmt.Sprintf("=%02X", c)
			} else {
				tok = string(c)
			}
		case c >= 33 && c <= 126:
			tok = string(c)
		default:
			tok = fmt.Sprintf("=%02X", c)
		}
		if col+len(tok) > 76 && col > 0 {
			out.WriteString("=\r\n")
			col = 0
		}
		out.WriteString(tok)
		col += len(tok)
	}
	return out.Bytes()
}
