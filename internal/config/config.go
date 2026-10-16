package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type Config struct {
	ProxyURL          string   `json:"proxy_url,omitempty"`
	Server            string   `json:"server"`
	Username          string   `json:"username"`
	Auth              string   `json:"auth"`
	CredentialCommand []string `json:"credential_command"`
	CAFile            string   `json:"ca_file,omitempty"`
	Maildir           string   `json:"maildir"`
	Folders           []string `json:"folders"`
	FilterFolders     []string `json:"filter_folders,omitempty"`
	Sieve             string   `json:"sieve,omitempty"`
	Interval          string   `json:"interval"`
	Idle              bool     `json:"idle"`
	RemoteWrites      bool     `json:"remote_writes"`
	MaxMessageBytes   int64    `json:"max_message_bytes"`
	Timeout           string   `json:"timeout"`
}

func Folder(name string) error {
	if name == "" || strings.ContainsAny(name, "\\\x00\r\n") || strings.HasPrefix(name, "/") {
		return fmt.Errorf("unsafe mailbox name %q", name)
	}
	for _, s := range strings.Split(name, "/") {
		if s == "" || s == "." || s == ".." || strings.HasPrefix(s, ".") || s == "new" || s == "cur" || s == "tmp" {
			return fmt.Errorf("mailbox name cannot be represented safely: %q", name)
		}
	}
	return nil
}
func Load(path string) (Config, error) {
	var c Config
	data, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer data.Close()
	d := json.NewDecoder(io.LimitReader(data, 1<<20))
	d.DisallowUnknownFields()
	if err = d.Decode(&c); err != nil {
		return c, err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return c, errors.New("configuration must contain exactly one JSON object")
	}
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return c, err
	}
	for _, p := range []*string{&c.Maildir, &c.CAFile, &c.Sieve} {
		if *p == "" {
			continue
		}
		if strings.HasPrefix(*p, "~/") {
			home, e := os.UserHomeDir()
			if e != nil {
				return c, e
			}
			*p = filepath.Join(home, (*p)[2:])
		}
		if !filepath.IsAbs(*p) {
			*p = filepath.Join(base, *p)
		}
		*p = filepath.Clean(*p)
	}
	return c, c.Validate()
}
func (c *Config) Validate() error {
	if c.ProxyURL != "" {
		u, err := url.Parse(c.ProxyURL)
		if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
			return errors.New("proxy_url must be an http or https proxy URL without credentials, query, or path")
		}
	}

	if _, _, err := net.SplitHostPort(c.Server); err != nil {
		return fmt.Errorf("server must be host:port: %w", err)
	}
	if strings.ContainsAny(c.Username, "\r\n\x00\x01") {
		return errors.New("username contains control characters")
	}
	if c.Username == "" || len(c.CredentialCommand) == 0 || c.CredentialCommand[0] == "" {
		return errors.New("username and credential_command are required")
	}
	if c.Auth == "" {
		c.Auth = "xoauth2"
	}
	if c.Auth != "xoauth2" && c.Auth != "plain" {
		return errors.New("auth must be xoauth2 or plain (over TLS)")
	}
	if c.Maildir == "" || !filepath.IsAbs(c.Maildir) {
		return errors.New("an absolute maildir path is required")
	}
	if len(c.Folders) == 0 {
		return errors.New("at least one folder or folder pattern is required")
	}
	seen := map[string]bool{}
	for _, f := range c.Folders {
		if err := Folder(f); err != nil {
			return err
		}
		if seen[f] {
			return fmt.Errorf("duplicate folder %q", f)
		}
		seen[f] = true
	}
	for _, f := range c.FilterFolders {
		if err := Folder(f); err != nil {
			return err
		}
		if strings.ContainsAny(f, "*%") {
			return errors.New("filter_folders requires literal mailbox names")
		}
		if !c.SelectsFolder(f) {
			return fmt.Errorf("filter folder %q must be in folders", f)
		}
	}
	if c.Interval == "" {
		c.Interval = "30s"
	}
	v, err := time.ParseDuration(c.Interval)
	if err != nil || v < time.Second {
		return errors.New("interval must be a duration of at least 1s")
	}
	if c.Timeout == "" {
		c.Timeout = "2m"
	}
	v, err = time.ParseDuration(c.Timeout)
	if err != nil || v < time.Second {
		return errors.New("timeout must be a duration of at least 1s")
	}
	if c.MaxMessageBytes == 0 {
		c.MaxMessageBytes = 64 << 20
	}
	if c.MaxMessageBytes < 1 {
		return errors.New("max_message_bytes must be positive")
	}
	return nil
}

// StateDirectory is reserved for private sync metadata, never a mail folder.
const StateDirectory = ".imaplocal"

func (c Config) StateDir() string              { return filepath.Join(c.Maildir, StateDirectory) }
func (c Config) PollInterval() time.Duration   { d, _ := time.ParseDuration(c.Interval); return d }
func (c Config) CommandTimeout() time.Duration { d, _ := time.ParseDuration(c.Timeout); return d }

// FolderPattern reports whether a selector needs remote LIST discovery.
func FolderPattern(pattern string) bool { return strings.ContainsAny(pattern, "*%") }

func (c Config) NeedsDiscovery() bool {
	for _, pattern := range c.Folders {
		if FolderPattern(pattern) {
			return true
		}
	}
	return false
}

// SelectsFolder uses IMAP wildcards: * crosses hierarchy boundaries and %
// matches within one level. All other characters are literal (not shell globs).
func (c Config) SelectsFolder(name string) bool {
	for _, pattern := range c.Folders {
		if !FolderPattern(pattern) {
			if pattern == name {
				return true
			}
			continue
		}
		expr := regexp.QuoteMeta(pattern)
		expr = strings.ReplaceAll(expr, `\*`, `.*`)
		expr = strings.ReplaceAll(expr, `%`, `[^/]*`)
		if ok, _ := regexp.MatchString(`\A`+expr+`\z`, name); ok {
			return true
		}
	}
	return false
}
