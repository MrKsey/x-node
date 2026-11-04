package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	errInviteInvalid = errors.New("invite is not valid")
	errInviteExpired = errors.New("invite has expired")
	errInviteUsed    = errors.New("invite has already been used")
	errAllowlistFull = errors.New("the allowlist is full")
	errNotAllowed    = errors.New("the address is not in the allowlist")
)

// Client is one node allowed to use the exit.
type Client struct {
	Address      string    `json:"address"`
	PublicKey    string    `json:"publicKey,omitempty"`
	Label        string    `json:"label"`
	DeviceName   string    `json:"deviceName,omitempty"`
	AppVersion   string    `json:"appVersion,omitempty"`
	Source       string    `json:"source"`
	RegisteredAt time.Time `json:"registeredAt"`
	LastSeenAt   time.Time `json:"lastSeenAt"`

	// TokenHash is the SHA-256 of the API token of this client. Only the hash
	// is stored: a leaked allowlist file must not hand out a working
	// credential. The token itself is shown once, when it is issued.
	TokenHash string `json:"tokenHash,omitempty"`
	// TokenIssuedAt tells the operator when the current token was handed out.
	// A missing timestamp with a hash means the file was edited by hand.
	TokenIssuedAt time.Time `json:"tokenIssuedAt,omitempty"`
}

// Invite is a one-time (or N-time) registration secret. The hash is what the
// registration path compares; the token is kept as well, so the web interface
// and "x-node invite list" can show the link again instead of forcing the
// operator to create a new invite for a client that lost the QR code. The file
// lives in the state directory, which only the service user and root can read.
type Invite struct {
	Hash      string    `json:"hash"`
	Note      string    `json:"note"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
	Uses      int       `json:"uses"`
	Used      int       `json:"used"`
	Token     string    `json:"token,omitempty"`
}

type allowlistFile struct {
	Clients []Client `json:"clients"`
}

type inviteFile struct {
	Invites []Invite `json:"invites"`
}

// fileStamp is what the store remembers about a file it has loaded. When the
// stamp on disk differs, another process (the CLI) has changed the file and the
// service reloads it before the next operation.
type fileStamp struct {
	modTime time.Time
	size    int64
}

func stampOf(path string) fileStamp {
	info, err := os.Stat(path)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{modTime: info.ModTime(), size: info.Size()}
}

// Store keeps the allowlist and the invites in two small JSON files. Writes
// are atomic (temp file + rename), so a crash never leaves a half-written
// allowlist behind. The CLI is a separate process: every operation refreshes
// the in-memory copy when the files changed, and every write takes a
// cross-process lock, so `x-node invite` and `x-node allow` work while the
// service is running.
type Store struct {
	mu          sync.Mutex
	dir         string
	allowPath   string
	invitePath  string
	clients     map[string]Client
	invites     map[string]Invite
	allowStamp  fileStamp
	inviteStamp fileStamp
	log         *logger
}

func openStore(cfg Config, log *logger) (*Store, error) {
	dir := cfg.StateDir
	if strings.TrimSpace(dir) == "" {
		dir = defaultStateDir
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	// Create the lock file as the service user, so a later root CLI run does
	// not leave a root-only file that the service cannot open.
	lockPath := filepath.Join(dir, "store.lock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create store lock: %w", err)
	}
	_ = lock.Close()
	chownToDir(lockPath, dir)
	s := &Store{
		dir:        dir,
		allowPath:  filepath.Join(dir, "allowlist.json"),
		invitePath: filepath.Join(dir, "invites.json"),
		clients:    make(map[string]Client),
		invites:    make(map[string]Invite),
		log:        log,
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	if err := s.loadAllowlistLocked(); err != nil {
		return err
	}
	if err := s.loadInvitesLocked(); err != nil {
		return err
	}
	return nil
}

func (s *Store) loadAllowlistLocked() error {
	var allow allowlistFile
	if raw, err := os.ReadFile(s.allowPath); err == nil {
		if err := json.Unmarshal(raw, &allow); err != nil {
			return fmt.Errorf("parse allowlist: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read allowlist: %w", err)
	}
	s.clients = make(map[string]Client, len(allow.Clients))
	for _, client := range allow.Clients {
		ip := net.ParseIP(client.Address)
		if ip == nil {
			s.log.warn("allowlist: skipping invalid address %q", client.Address)
			continue
		}
		client.Address = ip.String()
		s.clients[client.Address] = client
	}
	s.allowStamp = stampOf(s.allowPath)
	return nil
}

func (s *Store) loadInvitesLocked() error {
	var invites inviteFile
	if raw, err := os.ReadFile(s.invitePath); err == nil {
		if err := json.Unmarshal(raw, &invites); err != nil {
			return fmt.Errorf("parse invites: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read invites: %w", err)
	}
	s.invites = make(map[string]Invite, len(invites.Invites))
	now := time.Now()
	for _, invite := range invites.Invites {
		if invite.Hash == "" || invite.Used >= invite.Uses || now.After(invite.ExpiresAt) {
			continue
		}
		s.invites[invite.Hash] = invite
	}
	s.inviteStamp = stampOf(s.invitePath)
	return nil
}

// refreshLocked picks up files written by the CLI. The caller holds s.mu.
func (s *Store) refreshLocked() {
	if stampOf(s.allowPath) != s.allowStamp {
		if err := s.loadAllowlistLocked(); err != nil {
			s.log.warn("could not reload allowlist: %v", err)
			s.allowStamp = stampOf(s.allowPath)
		}
	}
	if stampOf(s.invitePath) != s.inviteStamp {
		if err := s.loadInvitesLocked(); err != nil {
			s.log.warn("could not reload invites: %v", err)
			s.inviteStamp = stampOf(s.invitePath)
		}
	}
}

func (s *Store) saveAllowlistLocked() error {
	clients := make([]Client, 0, len(s.clients))
	for _, client := range s.clients {
		clients = append(clients, client)
	}
	sort.Slice(clients, func(i, j int) bool { return clients[i].Address < clients[j].Address })
	if err := writeJSONAtomic(s.allowPath, allowlistFile{Clients: clients}); err != nil {
		return err
	}
	s.allowStamp = stampOf(s.allowPath)
	return nil
}

func (s *Store) saveInvitesLocked() error {
	invites := make([]Invite, 0, len(s.invites))
	for _, invite := range s.invites {
		invites = append(invites, invite)
	}
	sort.Slice(invites, func(i, j int) bool { return invites[i].CreatedAt.Before(invites[j].CreatedAt) })
	if err := writeJSONAtomic(s.invitePath, inviteFile{Invites: invites}); err != nil {
		return err
	}
	s.inviteStamp = stampOf(s.invitePath)
	return nil
}

func writeJSONAtomic(path string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	chownToDir(tmpName, filepath.Dir(path))
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// beginWrite serialises a read-modify-write cycle with the CLI: it takes the
// in-process mutex, the cross-process file lock and refreshes the state from
// disk. The returned function releases both locks.
func (s *Store) beginWrite() (func(), error) {
	s.mu.Lock()
	unlock, err := lockFile(filepath.Join(s.dir, "store.lock"))
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	s.refreshLocked()
	return func() {
		unlock()
		s.mu.Unlock()
	}, nil
}

// Allowed reports whether the node address is in the allowlist.
func (s *Store) Allowed(ip net.IP) bool {
	if ip == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	_, ok := s.clients[ip.String()]
	return ok
}

// Get returns one allowlist entry.
func (s *Store) Get(ip net.IP) (Client, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	client, ok := s.clients[ip.String()]
	return client, ok
}

// List returns every allowlist entry, sorted by address.
func (s *Store) List() []Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	out := make([]Client, 0, len(s.clients))
	for _, client := range s.clients {
		out = append(out, client)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Address < out[j].Address })
	return out
}

// AddClient inserts or updates one entry by hand (the CLI path).
func (s *Store) AddClient(ip net.IP, label string) (Client, bool, error) {
	if ip == nil || !isYggAddress(ip) {
		return Client{}, false, fmt.Errorf("address must be a Yggdrasil node address")
	}
	release, err := s.beginWrite()
	if err != nil {
		return Client{}, false, err
	}
	defer release()
	key := ip.String()
	now := time.Now()
	if existing, ok := s.clients[key]; ok {
		if label != "" {
			existing.Label = label
		}
		existing.Source = "manual"
		s.clients[key] = existing
		if err := s.saveAllowlistLocked(); err != nil {
			return Client{}, true, err
		}
		return existing, true, nil
	}
	client := Client{
		Address:      key,
		Label:        label,
		Source:       "manual",
		RegisteredAt: now,
		LastSeenAt:   now,
	}
	s.clients[key] = client
	if err := s.saveAllowlistLocked(); err != nil {
		delete(s.clients, key)
		return Client{}, false, err
	}
	return client, false, nil
}

// UpdateClient changes the editable fields of one entry. The address is the
// key and cannot be changed here.
func (s *Store) UpdateClient(ip net.IP, label string) (Client, error) {
	if ip == nil || !isYggAddress(ip) {
		return Client{}, fmt.Errorf("address must be a Yggdrasil node address")
	}
	release, err := s.beginWrite()
	if err != nil {
		return Client{}, err
	}
	defer release()
	key := ip.String()
	client, ok := s.clients[key]
	if !ok {
		return Client{}, errNotAllowed
	}
	client.Label = strings.TrimSpace(label)
	s.clients[key] = client
	if err := s.saveAllowlistLocked(); err != nil {
		return Client{}, err
	}
	return client, nil
}

// RemoveClient deletes one entry. It reports whether anything was removed.
func (s *Store) RemoveClient(ip net.IP) (bool, error) {
	if ip == nil {
		return false, fmt.Errorf("invalid address")
	}
	release, err := s.beginWrite()
	if err != nil {
		return false, err
	}
	defer release()
	key := ip.String()
	if _, ok := s.clients[key]; !ok {
		return false, nil
	}
	delete(s.clients, key)
	return true, s.saveAllowlistLocked()
}

// Touch records that an allowed client is still active. The write is skipped
// for a while so a busy client does not rewrite the file on every connection.
func (s *Store) Touch(ip net.IP) {
	if ip == nil {
		return
	}
	release, err := s.beginWrite()
	if err != nil {
		s.log.warn("could not lock the store: %v", err)
		return
	}
	defer release()
	key := ip.String()
	client, ok := s.clients[key]
	if !ok {
		return
	}
	now := time.Now()
	if now.Sub(client.LastSeenAt) < 10*time.Minute {
		client.LastSeenAt = now
		s.clients[key] = client
		return
	}
	client.LastSeenAt = now
	s.clients[key] = client
	if err := s.saveAllowlistLocked(); err != nil {
		s.log.warn("could not save allowlist: %v", err)
	}
}

// NewInvite creates a registration secret and returns the token and its stored
// form. The token is kept next to its hash so the operator can show the link
// again; the state directory is readable only by the service user and root.
func (s *Store) NewInvite(note string, ttl time.Duration, uses int) (string, Invite, error) {
	if uses <= 0 {
		uses = 1
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", Invite{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	now := time.Now()
	invite := Invite{
		Hash:      hex.EncodeToString(sum[:]),
		Note:      note,
		CreatedAt: now,
		ExpiresAt: now.Add(ttl),
		Uses:      uses,
		Token:     token,
	}
	release, err := s.beginWrite()
	if err != nil {
		return "", Invite{}, err
	}
	defer release()
	s.invites[invite.Hash] = invite
	if err := s.saveInvitesLocked(); err != nil {
		delete(s.invites, invite.Hash)
		return "", Invite{}, err
	}
	return token, invite, nil
}

// DeleteInvite removes an invite by its stored hash. The web interface shows
// the hash as the identifier of an invite, because the token itself is never
// stored.
func (s *Store) DeleteInvite(hash string) (bool, error) {
	release, err := s.beginWrite()
	if err != nil {
		return false, err
	}
	defer release()
	if _, ok := s.invites[hash]; !ok {
		return false, nil
	}
	delete(s.invites, hash)
	return true, s.saveInvitesLocked()
}

// newAPIToken returns a fresh API token and the SHA-256 of its stored form.
// The token is 32 random bytes in URL-safe base64, which is short enough for a
// QR code and long enough that guessing is not a concern.
func newAPIToken() (string, string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	return token, hashAPIToken(token), nil
}

func hashAPIToken(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(sum[:])
}

// ListInvites returns the invites that are still usable.
func (s *Store) ListInvites() []Invite {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	now := time.Now()
	out := make([]Invite, 0, len(s.invites))
	for _, invite := range s.invites {
		if invite.Used < invite.Uses && now.Before(invite.ExpiresAt) {
			out = append(out, invite)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// Register consumes an invite and adds the source address to the allowlist.
// Registering an address that is already allowed succeeds without consuming
// the invite: a lost HTTP response must not burn the token.
func (s *Store) Register(token string, client Client, maxClients int) (Client, bool, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return Client{}, false, errInviteInvalid
	}
	ip := net.ParseIP(client.Address)
	if ip == nil || !isYggAddress(ip) {
		return Client{}, false, fmt.Errorf("address must be a Yggdrasil node address")
	}
	client.Address = ip.String()
	if client.Label == "" {
		client.Label = client.DeviceName
	}
	if client.Label == "" {
		client.Label = client.Address
	}

	sum := sha256.Sum256([]byte(token))
	hash := hex.EncodeToString(sum[:])

	release, err := s.beginWrite()
	if err != nil {
		return Client{}, false, err
	}
	defer release()

	if existing, ok := s.clients[client.Address]; ok {
		existing.LastSeenAt = time.Now()
		if client.PublicKey != "" {
			existing.PublicKey = client.PublicKey
		}
		if client.DeviceName != "" {
			existing.DeviceName = client.DeviceName
		}
		if client.AppVersion != "" {
			existing.AppVersion = client.AppVersion
		}
		s.clients[client.Address] = existing
		if err := s.saveAllowlistLocked(); err != nil {
			return Client{}, false, err
		}
		return existing, true, nil
	}

	invite, ok := s.invites[hash]
	if !ok {
		return Client{}, false, errInviteInvalid
	}
	now := time.Now()
	if now.After(invite.ExpiresAt) {
		delete(s.invites, hash)
		_ = s.saveInvitesLocked()
		return Client{}, false, errInviteExpired
	}
	if invite.Used >= invite.Uses {
		delete(s.invites, hash)
		_ = s.saveInvitesLocked()
		return Client{}, false, errInviteUsed
	}
	if maxClients > 0 && len(s.clients) >= maxClients {
		return Client{}, false, errAllowlistFull
	}

	client.Source = "invite"
	client.RegisteredAt = now
	client.LastSeenAt = now
	s.clients[client.Address] = client

	invite.Used++
	if invite.Used >= invite.Uses {
		delete(s.invites, hash)
	} else {
		s.invites[hash] = invite
	}
	if err := s.saveAllowlistLocked(); err != nil {
		delete(s.clients, client.Address)
		return Client{}, false, err
	}
	if err := s.saveInvitesLocked(); err != nil {
		s.log.warn("allowlist saved but invites were not: %v", err)
	}
	return client, false, nil
}

// IssueToken replaces the API token of an allowlisted client and returns the
// new token. It is the recovery path: the Yggdrasil source address of the
// request already proves who is asking, so a reinstalled app or a profile that
// predates tokens can get access again without a new invite. Only the hash is
// stored; the previous token stops working immediately.
func (s *Store) IssueToken(ip net.IP) (string, error) {
	if ip == nil || !isYggAddress(ip) {
		return "", fmt.Errorf("address must be a Yggdrasil node address")
	}
	release, err := s.beginWrite()
	if err != nil {
		return "", err
	}
	defer release()
	key := ip.String()
	client, ok := s.clients[key]
	if !ok {
		return "", errNotAllowed
	}
	token, hash, err := newAPIToken()
	if err != nil {
		return "", err
	}
	client.TokenHash = hash
	client.TokenIssuedAt = time.Now()
	s.clients[key] = client
	if err := s.saveAllowlistLocked(); err != nil {
		return "", err
	}
	return token, nil
}

// TokenValid reports whether token is the API token of the client at ip. The
// comparison is constant-time, and an address without a token never matches.
func (s *Store) TokenValid(ip net.IP, token string) bool {
	if ip == nil || strings.TrimSpace(token) == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	client, ok := s.clients[ip.String()]
	if !ok || client.TokenHash == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(client.TokenHash), []byte(hashAPIToken(token))) == 1
}

// Cleanup drops expired invites. Called by the serve loop now and then.
func (s *Store) Cleanup() {
	release, err := s.beginWrite()
	if err != nil {
		s.log.warn("could not lock the store: %v", err)
		return
	}
	defer release()
	now := time.Now()
	changed := false
	for hash, invite := range s.invites {
		if invite.Used >= invite.Uses || now.After(invite.ExpiresAt) {
			delete(s.invites, hash)
			changed = true
		}
	}
	if changed {
		if err := s.saveInvitesLocked(); err != nil {
			s.log.warn("could not save invites: %v", err)
		}
	}
}
