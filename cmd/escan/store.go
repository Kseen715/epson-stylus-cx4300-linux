package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hirochachacha/go-smb2"
)

// validName reports whether name is a plain file name, safe to join onto a
// local directory or an SMB path. Neither store may be asked to escape its own
// directory, and the checks that look sufficient are not: filepath.Base only
// splits on the separator of the machine escan runs on, so on Linux it happily
// passes `..\..\secret.png` straight through to the backslash-separated path an
// SMB share uses. So the rule is an allowlist rather than a search for the
// traversal of the day - a name is exactly what scan files are named, and
// anything else is refused.
func validName(name string) bool {
	if name == "" || len(name) > 255 || strings.HasPrefix(name, ".") {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// store is where finished scans are written, and read back from when the
// browser asks for one at full resolution. Two implementations: the local
// filesystem, and an SMB share escan talks to itself.
type store interface {
	Create(name string) (io.WriteCloser, error)
	Open(name string) (io.ReadSeekCloser, time.Time, error)
	// Describe returns the location to show in the UI. It never contains a
	// password.
	Describe() string
}

// openStore picks the destination for finished scans: an SMB share when one is
// configured, the local directory otherwise. An SMB share is proven reachable
// here so a wrong address or password fails at startup, not after a scan that
// took minutes.
func openStore(smbAddress, smbUser, smbPassword, smbDomain, dir string) (store, error) {
	if smbAddress == "" {
		return newLocalStore(dir)
	}
	st, err := newSMBStore(smbAddress, smbUser, smbPassword, smbDomain)
	if err != nil {
		return nil, err
	}
	if err := st.check(); err != nil {
		return nil, err
	}
	return st, nil
}

type localStore struct{ dir string }

func newLocalStore(dir string) (*localStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("cannot use output directory %s: %w", dir, err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	return &localStore{dir: abs}, nil
}

// errBadName is returned rather than a path error, so nothing about the layout
// of the output directory leaks back to whoever asked for the odd name.
var errBadName = errors.New("not a valid file name")

func (l *localStore) Create(name string) (io.WriteCloser, error) {
	if !validName(name) {
		return nil, errBadName
	}
	return os.Create(filepath.Join(l.dir, name))
}

func (l *localStore) Open(name string) (io.ReadSeekCloser, time.Time, error) {
	if !validName(name) {
		return nil, time.Time{}, errBadName
	}
	f, err := os.Open(filepath.Join(l.dir, name))
	if err != nil {
		return nil, time.Time{}, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, time.Time{}, err
	}
	return f, fi.ModTime(), nil
}

func (l *localStore) Describe() string { return l.dir }

// smbStore writes scans straight to an SMB share. escan speaks SMB itself
// rather than leaning on a cifs mount, so it needs no root, no cifs-utils and
// no mount unit: the share is configured in /etc/escan.conf and nowhere else.
//
// One connection per operation. A scan takes minutes and a download is rare,
// so session setup costs nothing worth caching, and there is no idle session
// for the server to drop and for us to discover half-way through a write.
type smbStore struct {
	host   string // host:port
	share  string
	dir    string // subdirectory within the share; "" is the share root
	user   string
	pass   string
	domain string
}

// newSMBStore parses //host[:port]/share[/subdir]. Backslashes and an smb://
// prefix are accepted too, since that is how the same address gets written in
// Windows and in a browser.
func newSMBStore(address, user, pass, domain string) (*smbStore, error) {
	a := strings.ReplaceAll(address, `\`, "/")
	a = strings.TrimPrefix(a, "smb:")
	parts := strings.Split(strings.Trim(a, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("smb-address %q: expected //host/share or //host/share/subdir", address)
	}
	if user == "" {
		return nil, fmt.Errorf("smb-address is set but smb-user is not")
	}
	host := parts[0]
	// A bare IPv6 literal would be ambiguous here; it has to be written in
	// brackets, as [::1]:445, which this test then leaves alone.
	if _, _, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(host, "445")
	}
	return &smbStore{
		host:   host,
		share:  parts[1],
		dir:    strings.Join(parts[2:], `\`),
		user:   user,
		pass:   pass,
		domain: domain,
	}, nil
}

// smbSession is the three-layer stack a single operation needs, so it can be
// closed again as one thing.
type smbSession struct {
	conn  net.Conn
	sess  *smb2.Session
	share *smb2.Share
}

func (s *smbStore) connect() (*smbSession, error) {
	conn, err := net.DialTimeout("tcp", s.host, 15*time.Second)
	if err != nil {
		return nil, fmt.Errorf("smb: connecting to %s: %w", s.host, err)
	}
	d := &smb2.Dialer{Initiator: &smb2.NTLMInitiator{
		User: s.user, Password: s.pass, Domain: s.domain,
	}}
	sess, err := d.Dial(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("smb: logging in to %s as %s: %w", s.host, s.user, err)
	}
	share, err := sess.Mount(s.share)
	if err != nil {
		sess.Logoff()
		conn.Close()
		return nil, fmt.Errorf("smb: mounting share %q: %w", s.share, err)
	}
	return &smbSession{conn: conn, sess: sess, share: share}, nil
}

// Close tears the stack down innermost first, keeping the first error.
// Logoff closes the TCP connection itself, so the Close here is only for the
// case where logoff failed before getting that far; an already-closed
// connection is the normal outcome, not an error.
func (c *smbSession) Close() error {
	err := c.share.Umount()
	if e := c.sess.Logoff(); err == nil {
		err = e
	}
	if e := c.conn.Close(); err == nil && !errors.Is(e, net.ErrClosed) {
		err = e
	}
	return err
}

// smbFile holds its session open for as long as the caller holds the file.
type smbFile struct {
	*smb2.File
	sess *smbSession
}

func (f *smbFile) Close() error {
	err := f.File.Close()
	if e := f.sess.Close(); err == nil {
		err = e
	}
	return err
}

func (s *smbStore) Create(name string) (io.WriteCloser, error) {
	if !validName(name) {
		return nil, errBadName
	}
	c, err := s.connect()
	if err != nil {
		return nil, err
	}
	f, err := c.share.Create(s.remotePath(name))
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("smb: creating %s: %w", s.remotePath(name), err)
	}
	return &smbFile{File: f, sess: c}, nil
}

func (s *smbStore) Open(name string) (io.ReadSeekCloser, time.Time, error) {
	if !validName(name) {
		return nil, time.Time{}, errBadName
	}
	c, err := s.connect()
	if err != nil {
		return nil, time.Time{}, err
	}
	f, err := c.share.Open(s.remotePath(name))
	if err != nil {
		c.Close()
		return nil, time.Time{}, fmt.Errorf("smb: opening %s: %w", s.remotePath(name), err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		c.Close()
		return nil, time.Time{}, err
	}
	return &smbFile{File: f, sess: c}, fi.ModTime(), nil
}

// check proves the address and credentials work, so a bad configuration is a
// startup failure rather than a scan that runs for minutes and then cannot be
// saved.
func (s *smbStore) check() error {
	c, err := s.connect()
	if err != nil {
		return err
	}
	return c.Close()
}

func (s *smbStore) remotePath(name string) string {
	if s.dir == "" {
		return name
	}
	return s.dir + `\` + name
}

func (s *smbStore) Describe() string {
	host := strings.TrimSuffix(s.host, ":445")
	out := "//" + host + "/" + s.share
	if s.dir != "" {
		out += "/" + strings.ReplaceAll(s.dir, `\`, "/")
	}
	return out
}
