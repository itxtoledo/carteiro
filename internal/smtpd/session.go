package smtpd

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"carteiro/internal/storage"
)

const (
	maxCommandLine = 8192
	idleTimeout    = 5 * time.Minute
	dataTimeout    = 15 * time.Minute
	authTimeout    = time.Minute
	handshakeTmo   = 30 * time.Second
)

var errLineTooLong = errors.New("command line too long")

var connCounter atomic.Uint64

type smtpConn struct {
	s      *Server
	id     uint64
	nc     net.Conn
	remote string
	br     *bufio.Reader
	bw     *bufio.Writer

	helo   string
	from   string
	rcpts  []string
	user   *storage.Account
	tlsOn  bool
	closer bool
}

func (s *Server) serveConn(conn net.Conn) {
	defer conn.Close()
	c := &smtpConn{
		s:      s,
		id:     connCounter.Add(1),
		nc:     conn,
		remote: remoteString(conn.RemoteAddr()),
	}

	if s.tlsCfg != nil && s.cfg.TLS.Mode == "implicit" {
		tlsConn := tls.Server(conn, s.tlsCfg)
		conn.SetDeadline(time.Now().Add(handshakeTmo))
		if err := tlsConn.Handshake(); err != nil {
			s.log.Debug("implicit TLS handshake failed", "remote", c.remote, "err", err)
			return
		}
		conn.SetDeadline(time.Time{})
		c.nc = tlsConn
		c.tlsOn = true
	}

	c.br = bufio.NewReader(c.nc)
	c.bw = bufio.NewWriter(c.nc)

	s.log.Debug("connection opened", "conn", c.id, "remote", c.remote)
	defer func() {
		c.flush()
		s.log.Debug("connection closed", "conn", c.id, "remote", c.remote)
	}()

	c.reply(220, "%s ESMTP Carteiro", s.cfg.Hostname)
	for {
		line, err := c.readLine()
		if err != nil {
			if err == errLineTooLong {
				c.reply(500, "5.5.2 Line too long")
				continue
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				c.reply(421, "4.4.2 %s Service closing transmission channel (timeout)", s.cfg.Hostname)
				return
			}
			if err != io.EOF {
				s.log.Debug("error reading command", "conn", c.id, "err", err)
			}
			return
		}
		if line == "" {
			continue
		}
		verb, arg := splitCommand(line)
		if c.handle(verb, arg) {
			return
		}
	}
}

func (c *smtpConn) handle(verb, arg string) (closeConn bool) {
	s := c.s
	switch strings.ToUpper(verb) {
	case "HELO":
		if arg == "" {
			c.reply(501, "5.5.4 HELO requires a domain")
			return false
		}
		c.helo = arg
		c.reset(false)
		c.reply(250, "%s", s.cfg.Hostname)

	case "EHLO":
		if arg == "" {
			c.reply(501, "5.5.4 EHLO requires a domain")
			return false
		}
		c.helo = arg
		c.reset(false)
		c.sendEhlo()

	case "AUTH":
		return c.handleAuth(arg)

	case "MAIL":
		c.handleMail(arg)

	case "RCPT":
		c.handleRcpt(arg)

	case "DATA":
		c.handleData()

	case "RSET":
		c.reset(true)
		c.reply(250, "2.0.0 Ok")

	case "NOOP":
		c.reply(250, "2.0.0 Ok")

	case "VRFY":
		c.reply(252, "2.1.5 Cannot VRFY user, but will accept message")

	case "EXPN":
		c.reply(502, "5.5.1 Command not implemented")

	case "HELP":
		c.reply(214, "2.0.0 https://github.com/ (Carteiro SMTP relay)")

	case "QUIT":
		c.reply(221, "2.0.0 Bye")
		return true

	case "STARTTLS":
		c.handleStartTLS()

	case "BDAT":
		c.reply(502, "5.5.1 BDAT not supported, use DATA")

	default:
		c.reply(500, "5.5.2 Unrecognized command: %s", verb)
	}
	return false
}

func (c *smtpConn) sendEhlo() {
	s := c.s
	lines := []string{
		"250-" + s.cfg.Hostname,
		"250-PIPELINING",
		"250-8BITMIME",
		fmt.Sprintf("250-SIZE %d", s.cfg.MaxMessageSize),
	}
	if s.tlsCfg != nil && s.cfg.TLS.Mode == "starttls" && !c.tlsOn {
		lines = append(lines, "250-STARTTLS")
	}
	if c.authAllowed() {
		lines = append(lines, "250-AUTH PLAIN LOGIN")
	}
	// The last line of the EHLO reply must not have a '-' suffix.
	lines[len(lines)-1] = strings.Replace(lines[len(lines)-1], "250-", "250 ", 1)
	c.writeLines(lines...)
	c.flush()
}

func (c *smtpConn) handleStartTLS() {
	s := c.s
	if s.tlsCfg == nil || s.cfg.TLS.Mode != "starttls" {
		c.reply(502, "5.5.1 STARTTLS not available")
		return
	}
	if c.tlsOn {
		c.reply(503, "5.5.1 TLS already active")
		return
	}
	c.reply(220, "2.0.0 Ready to start TLS")
	c.flush()

	raw := c.nc
	tlsConn := tls.Server(raw, s.tlsCfg)
	raw.SetDeadline(time.Now().Add(handshakeTmo))
	if err := tlsConn.Handshake(); err != nil {
		s.log.Debug("STARTTLS handshake failed", "conn", c.id, "err", err)
		return
	}
	raw.SetDeadline(time.Time{})
	c.nc = tlsConn
	c.br = bufio.NewReader(c.nc)
	c.bw = bufio.NewWriter(c.nc)
	c.tlsOn = true
	c.helo = ""
	c.reset(false)
	s.log.Debug("tls established", "conn", c.id)
}

func (c *smtpConn) authAllowed() bool {
	if !c.s.cfg.RequireTLS {
		return true
	}
	if c.tlsOn {
		return true
	}
	return isLoopback(c.nc.RemoteAddr())
}

func (c *smtpConn) handleAuth(arg string) (closeConn bool) {
	if c.user != nil {
		c.reply(503, "5.5.1 Already authenticated")
		return false
	}
	if !c.authAllowed() {
		c.reply(538, "5.7.11 Authentication requires TLS (STARTTLS)")
		return false
	}
	method := strings.ToUpper(arg)
	if method == "LOGIN" {
		return c.authLogin()
	}
	if method == "PLAIN" {
		return c.authPlain("")
	}
	// "AUTH PLAIN <credentials>" or unknown mechanism
	fields := strings.Fields(arg)
	if len(fields) == 2 && strings.EqualFold(fields[0], "PLAIN") {
		return c.authPlain(fields[1])
	}
	c.reply(504, "5.5.4 Unsupported authentication mechanism")
	return false
}

func (c *smtpConn) authPlain(inline string) (closeConn bool) {
	payload := inline
	if payload == "" {
		if !c.reply334("") {
			return false
		}
		line, err := c.readLine()
		if err != nil {
			return true
		}
		payload = line
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(payload))
	if err != nil {
		c.reply(501, "5.5.2 Invalid base64")
		return false
	}
	parts := bytes.Split(decoded, []byte{0})
	if len(parts) != 3 {
		c.reply(501, "5.5.2 Invalid AUTH PLAIN format")
		return false
	}
	authzID := string(parts[0])
	authcID := string(parts[1])
	if authzID != "" && authzID != authcID {
		c.reply(535, "5.7.8 authorization identity not supported")
		return false
	}
	c.tryAuthenticate(authcID, string(parts[2]))
	return false
}

func (c *smtpConn) authLogin() (closeConn bool) {
	if !c.reply334("VXNlcm5hbWU6") { // "Username:"
		return false
	}
	line, err := c.readLine()
	if err != nil {
		return true
	}
	userB64, err := base64.StdEncoding.DecodeString(strings.TrimSpace(line))
	if err != nil {
		c.reply(501, "5.5.2 Invalid base64")
		return false
	}
	if !c.reply334("UGFzc3dvcmQ6") { // "Password:"
		return false
	}
	line, err = c.readLine()
	if err != nil {
		return true
	}
	passB64, err := base64.StdEncoding.DecodeString(strings.TrimSpace(line))
	if err != nil {
		c.reply(501, "5.5.2 Invalid base64")
		return false
	}
	c.tryAuthenticate(string(userB64), string(passB64))
	return false
}

func (c *smtpConn) reply334(prompt string) bool {
	c.nc.SetDeadline(time.Now().Add(authTimeout))
	if prompt == "" {
		c.reply(334, " ")
	} else {
		c.reply(334, "%s", prompt)
	}
	return true
}

func (c *smtpConn) tryAuthenticate(login, password string) {
	login = strings.ToLower(strings.TrimSpace(login))
	acc, ok, err := c.s.store.GetAccount(login)
	if err != nil {
		c.s.log.Error("account lookup failed", "conn", c.id, "user", login, "err", err)
		c.failAuth(login)
		return
	}
	if !ok || !storage.VerifyPassword(acc.PasswordHash, password) {
		c.s.metrics.AuthFailure.Add(1)
		_ = c.s.store.AddCounter("auth_failure", 1)
		c.failAuth(login)
		return
	}
	c.user = &acc
	c.reset(false)
	c.s.metrics.AuthSuccess.Add(1)
	_ = c.s.store.AddCounter("auth_success", 1)
	c.s.log.Info("authenticated", "conn", c.id, "remote", c.remote, "user", acc.Email, "tls", c.tlsOn)
	c.reply(235, "2.7.0 Authentication successful")
}

func (c *smtpConn) failAuth(login string) {
	c.user = nil
	c.reset(false)
	c.s.log.Info("authentication failed", "conn", c.id, "remote", c.remote, "user", login)
	c.reply(535, "5.7.8 Authentication credentials invalid")
}

func (c *smtpConn) handleMail(arg string) {
	s := c.s
	if c.user == nil {
		c.reply(530, "5.7.0 Authentication required")
		return
	}
	if !strings.HasPrefix(strings.ToUpper(arg), "FROM:") {
		c.reply(501, "5.5.4 Syntax: MAIL FROM:<address> [parameters]")
		return
	}
	rest := strings.TrimSpace(arg[5:])
	addr, params := splitEnvelope(rest)
	addr = strings.ToLower(strings.TrimSpace(addr))
	if !validEnvelopeAddr(addr) {
		c.reply(501, "5.1.7 Invalid sender address: %s", addr)
		return
	}
	if !c.user.AllowsFrom(addr) {
		c.reply(553, "5.7.1 Sender %s not allowed for account %s", addr, c.user.Email)
		return
	}
	if size := paramSize(params); size > 0 && size > s.cfg.MaxMessageSize {
		c.reply(552, "5.3.4 Message size exceeds the limit of %d bytes", s.cfg.MaxMessageSize)
		return
	}
	c.from = addr
	c.rcpts = nil
	c.reply(250, "2.1.0 Ok")
}

func (c *smtpConn) handleRcpt(arg string) {
	s := c.s
	if c.from == "" {
		c.reply(503, "5.5.1 Need MAIL before RCPT")
		return
	}
	if len(c.rcpts) >= s.cfg.MaxRecipients {
		c.reply(452, "4.5.3 Limit of %d recipients per message reached", s.cfg.MaxRecipients)
		return
	}
	if !strings.HasPrefix(strings.ToUpper(arg), "TO:") {
		c.reply(501, "5.5.4 Syntax: RCPT TO:<address> [parameters]")
		return
	}
	rest := strings.TrimSpace(arg[3:])
	addr, _ := splitEnvelope(rest)
	addr = strings.ToLower(strings.TrimSpace(addr))
	if !validEnvelopeAddr(addr) {
		c.reply(501, "5.1.3 Invalid recipient address: %s", addr)
		return
	}
	c.rcpts = append(c.rcpts, addr)
	c.reply(250, "2.1.5 Ok")
}

func (c *smtpConn) handleData() {
	s := c.s
	if c.user == nil {
		c.reply(530, "5.7.0 Authentication required")
		return
	}
	if c.from == "" || len(c.rcpts) == 0 {
		c.reply(503, "5.5.1 Need MAIL and RCPT before DATA")
		return
	}
	c.reply(354, "End data with <CR><LF>.<CR><LF>")
	c.flush()

	c.nc.SetDeadline(time.Now().Add(dataTimeout))
	data, over, err := c.readData(s.cfg.MaxMessageSize)
	c.nc.SetDeadline(time.Time{})
	if err != nil {
		c.s.log.Debug("error reading DATA", "conn", c.id, "err", err)
		return
	}
	if over {
		c.reply(552, "5.3.4 Message size exceeds fixed limit of %d bytes", s.cfg.MaxMessageSize)
		c.reset(true)
		return
	}

	id := storage.NewID(time.Now())
	msg := c.receivedHeader(id) + string(data)
	raw := []byte(msg)
	// Record before the DB insert so a fast delivery can never race ahead of
	// the recent-sends feed; the entry is dropped if the enqueue fails.
	if s.rec != nil {
		s.rec.Add(id, c.from, c.rcpts, raw)
	}
	qid, err := s.store.EnqueueWithID(id, c.from, c.rcpts, raw)
	if err != nil {
		if s.rec != nil {
			s.rec.Drop(id)
		}
		c.s.log.Error("failed to queue message", "conn", c.id, "err", err)
		c.reply(451, "4.3.0 Internal error while queueing; please retry")
		c.reset(true)
		return
	}
	s.metrics.MessagesQueued.Add(1)
	_ = s.store.AddCounter("messages_queued", 1)
	c.s.log.Info("message queued",
		"conn", c.id, "remote", c.remote, "user", c.user.Email,
		"from", c.from, "to", c.rcpts, "id", id, "bytes", len(data))
	c.reply(250, "2.0.0 Ok: queued as %s", qid)
	c.reset(true)
}

func (c *smtpConn) receivedHeader(msgID string) string {
	remoteIP := remoteIP(c.nc.RemoteAddr())
	helo := c.helo
	if helo == "" {
		helo = remoteIP
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Received: from %s by %s with ESMTPSA id %s", helo, c.s.cfg.Hostname, msgID)
	for _, to := range c.rcpts {
		b.WriteString("\r\n\tfor <" + to + ">")
	}
	fmt.Fprintf(&b, "\r\n\t; %s\r\n", time.Now().Format("Mon, 02 Jan 2006 15:04:05 -0700"))
	return b.String()
}

// readData consumes the message until the final "." line (RFC 5321,
// §4.1.1.4), undoing dot-stuffing. If the limit is exceeded, the remainder is
// discarded until the terminator and over=true.
func (c *smtpConn) readData(limit int64) ([]byte, bool, error) {
	var buf bytes.Buffer
	var n int64
	over := false
	for {
		line, err := c.readDataLine(over)
		if err != nil {
			return nil, over, err
		}
		n += int64(len(line)) + 1
		if n > limit {
			over = true
		}
		if line == "." {
			return buf.Bytes(), over, nil
		}
		if over {
			continue
		}
		if strings.HasPrefix(line, ".") {
			line = line[1:]
		}
		buf.WriteString(line)
		buf.WriteByte('\n')
	}
}

func (c *smtpConn) reset(clearAuth bool) {
	c.from = ""
	c.rcpts = nil
	if clearAuth {
		c.user = nil
	}
}

func (c *smtpConn) reply(code int, format string, args ...any) {
	c.writeLines(fmt.Sprintf("%d %s", code, fmt.Sprintf(format, args...)))
	c.flush()
}

func (c *smtpConn) writeLines(lines ...string) {
	for _, l := range lines {
		c.bw.WriteString(l)
		c.bw.WriteString("\r\n")
	}
}

func (c *smtpConn) flush() { c.bw.Flush() }

// readLine reads a command line with a size limit.
func (c *smtpConn) readLine() (string, error) {
	c.nc.SetReadDeadline(time.Now().Add(idleTimeout))
	line, err := c.readRaw(maxCommandLine)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// readDataLine reads one line of the message body. When discard=true the
// bytes are dropped without accumulating memory (limit-exceeded mode).
func (c *smtpConn) readDataLine(discard bool) (string, error) {
	mode := -1
	if discard {
		mode = 0
	}
	line, err := c.readRaw(mode)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// readRaw reads bytes until '\n' with three modes: mode < 0 accumulates
// everything, mode == 0 discards the bytes (no allocation) and mode > 0
// enforces the limit; past the limit the rest of the line is discarded and
// errLineTooLong is returned.
func (c *smtpConn) readRaw(mode int) (string, error) {
	var buf []byte
	for {
		frag, err := c.br.ReadSlice('\n')
		if mode < 0 || (mode > 0 && len(buf)+len(frag) <= mode) {
			buf = append(buf, frag...)
		}
		switch err {
		case nil:
			if mode > 0 && len(buf) > mode {
				return "", errLineTooLong
			}
			return string(buf), nil
		case bufio.ErrBufferFull:
			continue
		default:
			return string(buf), err
		}
	}
}

func splitCommand(line string) (verb, arg string) {
	line = strings.TrimLeft(line, " ")
	line = strings.TrimRight(line, " ")
	if i := strings.IndexAny(line, " \t"); i >= 0 {
		return line[:i], strings.TrimSpace(line[i+1:])
	}
	return line, ""
}

// splitEnvelope extracts the address (with or without <>) and the remaining
// parameters.
func splitEnvelope(s string) (addr, params string) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "<") {
		if end := strings.IndexByte(s, '>'); end >= 0 {
			return s[1:end], strings.TrimSpace(s[end+1:])
		}
		return strings.TrimPrefix(s, "<"), ""
	}
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return "", ""
	}
	return fields[0], strings.TrimSpace(strings.TrimPrefix(s, fields[0]))
}

func paramSize(params string) int64 {
	fields := strings.Fields(strings.ToUpper(params))
	for _, f := range fields {
		if rest, ok := strings.CutPrefix(f, "SIZE="); ok {
			n, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
			if err == nil && n >= 0 {
				return n
			}
		}
	}
	return 0
}

func validEnvelopeAddr(addr string) bool {
	at := strings.LastIndexByte(addr, '@')
	if at <= 0 || at == len(addr)-1 {
		return false
	}
	return !strings.ContainsAny(addr, " \t<>()")
}

func remoteString(addr net.Addr) string {
	if tcp, ok := addr.(*net.TCPAddr); ok {
		return tcp.IP.String()
	}
	return addr.String()
}

func remoteIP(addr net.Addr) string {
	ipStr := remoteString(addr)
	if strings.Contains(ipStr, ":") {
		return "[" + ipStr + "]"
	}
	return ipStr
}

func isLoopback(addr net.Addr) bool {
	tcp, ok := addr.(*net.TCPAddr)
	if !ok {
		return false
	}
	return tcp.IP.IsLoopback()
}
