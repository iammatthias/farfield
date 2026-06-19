package main

import (
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/iammatthias/farfield/lib/auth"
	"github.com/iammatthias/farfield/lib/store"
	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// Token kinds and states.
const (
	kindSelf  = "self"  // reusable, unlimited, never-expiring — my own installs
	kindShare = "share" // public single-use (or bounded) off-tailnet link

	stateActive   = "active"
	stateConsumed = "consumed"
	stateRevoked  = "revoked"
)

// consumeGrace keeps a token's .ipa bytes flowing for a short window after it
// flips to consumed, so iOS's trailing range requests inside one install can
// still finish. A forwarded link replayed after the window is refused.
const consumeGrace = 5 * time.Minute

// Build is one stored, content-addressed .ipa and its extracted metadata. ID is
// the short content address (cid(.ipa)[:16]); CID is the full identifier.
// Identical bytes collapse to one row.
type Build struct {
	ID            string `json:"id"`
	CID           string `json:"cid"`
	BundleID      string `json:"bundleId"`
	AppName       string `json:"appName"`
	Version       string `json:"version"`       // CFBundleShortVersionString
	BuildNumber   string `json:"buildNumber"`   // CFBundleVersion
	Team          string `json:"team"`          // provisioning TeamName
	ProfileExpiry string `json:"profileExpiry"` // RFC 3339 UTC, '' = unknown
	DeviceCount   int    `json:"deviceCount"`   // enrolled UDIDs in the profile
	UDIDs         string `json:"-"`             // newline-joined, never surfaced
	SizeBytes     int64  `json:"sizeBytes"`
	Filename      string `json:"filename"` // original upload name, for downloads
	GitCommit     string `json:"gitCommit,omitempty"`
	Notes         string `json:"notes,omitempty"`
	CreatedAt     string `json:"createdAt"`
}

// Token is an install-session capability. It authorizes the whole install
// (manifest, .ipa, icons) under /i/{token}/ for exactly one build.
type Token struct {
	Token        string `json:"token"`
	BuildID      string `json:"buildId"`
	Kind         string `json:"kind"`
	State        string `json:"state"`
	MaxInstalls  int    `json:"maxInstalls"` // 0 = unlimited
	UsedInstalls int    `json:"usedInstalls"`
	ExpiresAt    string `json:"expiresAt"` // RFC 3339 UTC, '' = never
	Label        string `json:"label"`
	LastUA       string `json:"-"`
	LastIP       string `json:"-"`
	CreatedAt    string `json:"createdAt"`
	ConsumedAt   string `json:"consumedAt,omitempty"`
}

// expired reports whether the token's TTL backstop has passed.
func (t *Token) expired() bool {
	return t.ExpiresAt != "" && t.ExpiresAt <= store.NowRFC3339()
}

// canStart reports whether a fresh install may BEGIN against this token — the
// gate on the manifest fetch and the share landing page.
func (t *Token) canStart() bool {
	return t.State == stateActive && !t.expired() &&
		(t.MaxInstalls == 0 || t.UsedInstalls < t.MaxInstalls)
}

// canServeBytes reports whether the .ipa (and icons) may still be served. It is
// looser than canStart by the grace window so an in-flight multi-range download
// finishes even after the token flips to consumed.
func (t *Token) canServeBytes() bool {
	if t.State == stateRevoked || t.expired() {
		return false
	}
	if t.canStart() {
		return true
	}
	if t.State == stateConsumed && t.ConsumedAt != "" {
		if c, err := time.Parse(time.RFC3339, t.ConsumedAt); err == nil {
			return time.Since(c) <= consumeGrace
		}
	}
	return false
}

// schema — builds plus their install tokens.
const schema = `
CREATE TABLE IF NOT EXISTS builds (
	id             TEXT PRIMARY KEY,
	cid            TEXT NOT NULL DEFAULT '',
	bundle_id      TEXT NOT NULL DEFAULT '',
	app_name       TEXT NOT NULL DEFAULT '',
	version        TEXT NOT NULL DEFAULT '',
	build_number   TEXT NOT NULL DEFAULT '',
	team           TEXT NOT NULL DEFAULT '',
	profile_expiry TEXT NOT NULL DEFAULT '',
	device_count   INTEGER NOT NULL DEFAULT 0,
	udids          TEXT NOT NULL DEFAULT '',
	size_bytes     INTEGER NOT NULL DEFAULT 0,
	filename       TEXT NOT NULL DEFAULT '',
	git_commit     TEXT NOT NULL DEFAULT '',
	notes          TEXT NOT NULL DEFAULT '',
	created_at     TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS builds_by_bundle
	ON builds (bundle_id, created_at DESC);

CREATE TABLE IF NOT EXISTS install_tokens (
	token         TEXT PRIMARY KEY,
	build_id      TEXT NOT NULL,
	kind          TEXT NOT NULL DEFAULT 'self',
	state         TEXT NOT NULL DEFAULT 'active',
	max_installs  INTEGER NOT NULL DEFAULT 0,
	used_installs INTEGER NOT NULL DEFAULT 0,
	expires_at    TEXT NOT NULL DEFAULT '',
	label         TEXT NOT NULL DEFAULT '',
	last_ua       TEXT NOT NULL DEFAULT '',
	last_ip       TEXT NOT NULL DEFAULT '',
	created_at    TEXT NOT NULL,
	consumed_at   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS tokens_by_build ON install_tokens (build_id);
CREATE INDEX IF NOT EXISTS tokens_by_kind_state
	ON install_tokens (kind, state);

-- Optional rich page content, keyed by bundle id. Apps with no row here render
-- the plain version list — the metadata is entirely additive.
CREATE TABLE IF NOT EXISTS app_meta (
	bundle_id   TEXT PRIMARY KEY,
	tagline     TEXT NOT NULL DEFAULT '',
	description TEXT NOT NULL DEFAULT '',
	updated_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS app_screenshots (
	id         TEXT PRIMARY KEY,
	bundle_id  TEXT NOT NULL,
	cid        TEXT NOT NULL,
	ext        TEXT NOT NULL DEFAULT '',
	mime       TEXT NOT NULL DEFAULT '',
	width      INTEGER NOT NULL DEFAULT 0,
	height     INTEGER NOT NULL DEFAULT 0,
	caption    TEXT NOT NULL DEFAULT '',
	sort       INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS screenshots_by_app
	ON app_screenshots (bundle_id, sort, created_at);`

// openDB opens the SQLite database, applies pragmas, runs the schema, and
// performs idempotent column-add migrations — every step safe on every startup
// (see the self-migrating-sqlite skill).
func openDB(path string) (*sql.DB, error) {
	db, err := store.OpenDB(path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	if _, err := db.Exec(store.SessionSchema); err != nil {
		return nil, err
	}
	for _, c := range []struct{ col, decl string }{
		{"git_commit", "TEXT NOT NULL DEFAULT ''"},
		{"notes", "TEXT NOT NULL DEFAULT ''"},
		{"filename", "TEXT NOT NULL DEFAULT ''"},
	} {
		if err := store.EnsureColumn(db, "builds", c.col, c.decl); err != nil {
			return nil, err
		}
	}
	return db, nil
}

// ── builds ───────────────────────────────────────────────────────────────────

const buildCols = `id, cid, bundle_id, app_name, version, build_number, team,
	profile_expiry, device_count, udids, size_bytes, filename, git_commit,
	notes, created_at`

type scanner interface{ Scan(...any) error }

func scanBuild(row scanner) (*Build, error) {
	var b Build
	if err := row.Scan(&b.ID, &b.CID, &b.BundleID, &b.AppName, &b.Version,
		&b.BuildNumber, &b.Team, &b.ProfileExpiry, &b.DeviceCount, &b.UDIDs,
		&b.SizeBytes, &b.Filename, &b.GitCommit, &b.Notes, &b.CreatedAt); err != nil {
		return nil, err
	}
	return &b, nil
}

// insertBuild stores a build under its content address. Identical bytes collapse
// to the existing row (INSERT OR IGNORE keeps the original created_at and notes).
// Reports whether this call created a new row.
func insertBuild(db *sql.DB, b *Build) (bool, error) {
	if b.CreatedAt == "" {
		b.CreatedAt = store.NowRFC3339()
	}
	res, err := db.Exec(`INSERT OR IGNORE INTO builds (`+buildCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		b.ID, b.CID, b.BundleID, b.AppName, b.Version, b.BuildNumber, b.Team,
		b.ProfileExpiry, b.DeviceCount, b.UDIDs, b.SizeBytes, b.Filename,
		b.GitCommit, b.Notes, b.CreatedAt)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func getBuild(db *sql.DB, id string) (*Build, error) {
	b, err := scanBuild(db.QueryRow(
		`SELECT `+buildCols+` FROM builds WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return b, nil
}

func collectBuilds(rows *sql.Rows) ([]Build, error) {
	defer rows.Close()
	out := []Build{}
	for rows.Next() {
		b, err := scanBuild(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *b)
	}
	return out, rows.Err()
}

// listBuilds returns every build, newest first. The rowid tiebreak makes
// "newest" mean most-recently-uploaded even when two builds share a created_at
// second — otherwise an id sort would pick an arbitrary one as latest.
func listBuilds(db *sql.DB) ([]Build, error) {
	rows, err := db.Query(`SELECT ` + buildCols + ` FROM builds
		ORDER BY created_at DESC, rowid DESC`)
	if err != nil {
		return nil, err
	}
	return collectBuilds(rows)
}

// listBuildsByBundle returns one app's builds, newest (most recently uploaded)
// first.
func listBuildsByBundle(db *sql.DB, bundleID string) ([]Build, error) {
	rows, err := db.Query(`SELECT `+buildCols+` FROM builds
		WHERE bundle_id = ? ORDER BY created_at DESC, rowid DESC`, bundleID)
	if err != nil {
		return nil, err
	}
	return collectBuilds(rows)
}

// App is one distinct application in the index — its latest build plus a count.
type App struct {
	BundleID string
	AppName  string
	Latest   Build
	Count    int
}

// listApps groups builds by bundle id, newest build representing each app.
func listApps(db *sql.DB) ([]App, error) {
	builds, err := listBuilds(db)
	if err != nil {
		return nil, err
	}
	order := []string{}
	byBundle := map[string]*App{}
	for _, b := range builds {
		a, ok := byBundle[b.BundleID]
		if !ok {
			a = &App{BundleID: b.BundleID, AppName: b.AppName, Latest: b}
			byBundle[b.BundleID] = a
			order = append(order, b.BundleID)
		}
		a.Count++
		if a.AppName == "" {
			a.AppName = b.AppName
		}
	}
	out := make([]App, 0, len(order))
	for _, id := range order {
		out = append(out, *byBundle[id])
	}
	return out, nil
}

// deleteBuild removes a build and its tokens. Reports whether it existed.
func deleteBuild(db *sql.DB, id string) (bool, error) {
	if _, err := db.Exec(`DELETE FROM install_tokens WHERE build_id = ?`, id); err != nil {
		return false, err
	}
	res, err := db.Exec(`DELETE FROM builds WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func countBuilds(db *sql.DB) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM builds`).Scan(&n)
	return n, err
}

// deleteApp removes every build of one bundle id, along with their tokens and
// its rich-page metadata + screenshots. It returns the build content addresses
// and the screenshots it deleted so the caller can drop their files, plus the
// number of versions removed.
func deleteApp(db *sql.DB, bundleID string) (cids []string, shots []Screenshot, n int, err error) {
	rows, err := db.Query(`SELECT cid FROM builds WHERE bundle_id = ?`, bundleID)
	if err != nil {
		return nil, nil, 0, err
	}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			rows.Close()
			return nil, nil, 0, err
		}
		cids = append(cids, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, 0, err
	}
	shots, err = listScreenshots(db, bundleID)
	if err != nil {
		return nil, nil, 0, err
	}
	if _, err := db.Exec(`DELETE FROM install_tokens WHERE build_id IN
		(SELECT id FROM builds WHERE bundle_id = ?)`, bundleID); err != nil {
		return nil, nil, 0, err
	}
	if _, err := db.Exec(`DELETE FROM app_screenshots WHERE bundle_id = ?`, bundleID); err != nil {
		return nil, nil, 0, err
	}
	if _, err := db.Exec(`DELETE FROM app_meta WHERE bundle_id = ?`, bundleID); err != nil {
		return nil, nil, 0, err
	}
	res, err := db.Exec(`DELETE FROM builds WHERE bundle_id = ?`, bundleID)
	if err != nil {
		return nil, nil, 0, err
	}
	affected, _ := res.RowsAffected()
	return cids, shots, int(affected), nil
}

// updateBuildNotes sets a build's changelog ("what's new") text.
func updateBuildNotes(db *sql.DB, id, notes string) error {
	_, err := db.Exec(`UPDATE builds SET notes = ? WHERE id = ?`, notes, id)
	return err
}

// ── app rich-page metadata ───────────────────────────────────────────────────

// AppMeta is the optional rich-page content for an app, keyed by bundle id.
type AppMeta struct {
	BundleID    string
	Tagline     string
	Description string // markdown
	UpdatedAt   string
}

// Screenshot is one uploaded image for an app's page.
type Screenshot struct {
	ID        string
	BundleID  string
	CID       string
	Ext       string
	Mime      string
	Width     int
	Height    int
	Caption   string
	Sort      int
	CreatedAt string
}

// getAppMeta returns the rich-page metadata for a bundle id, or nil when none
// has been set — the backwards-compatible default.
func getAppMeta(db *sql.DB, bundleID string) (*AppMeta, error) {
	var m AppMeta
	err := db.QueryRow(`SELECT bundle_id, tagline, description, updated_at
		FROM app_meta WHERE bundle_id = ?`, bundleID).Scan(
		&m.BundleID, &m.Tagline, &m.Description, &m.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// upsertAppMeta writes (or clears) an app's tagline and description. When both
// are empty the row is removed, so an app falls cleanly back to the plain view.
func upsertAppMeta(db *sql.DB, m *AppMeta) error {
	if strings.TrimSpace(m.Tagline) == "" && strings.TrimSpace(m.Description) == "" {
		_, err := db.Exec(`DELETE FROM app_meta WHERE bundle_id = ?`, m.BundleID)
		return err
	}
	_, err := db.Exec(`INSERT INTO app_meta (bundle_id, tagline, description, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(bundle_id) DO UPDATE SET
			tagline = excluded.tagline,
			description = excluded.description,
			updated_at = excluded.updated_at`,
		m.BundleID, m.Tagline, m.Description, store.NowRFC3339())
	return err
}

func scanScreenshot(row scanner) (*Screenshot, error) {
	var s Screenshot
	if err := row.Scan(&s.ID, &s.BundleID, &s.CID, &s.Ext, &s.Mime,
		&s.Width, &s.Height, &s.Caption, &s.Sort, &s.CreatedAt); err != nil {
		return nil, err
	}
	return &s, nil
}

const screenshotCols = `id, bundle_id, cid, ext, mime, width, height, caption, sort, created_at`

// listScreenshots returns an app's screenshots in display order.
func listScreenshots(db *sql.DB, bundleID string) ([]Screenshot, error) {
	rows, err := db.Query(`SELECT `+screenshotCols+` FROM app_screenshots
		WHERE bundle_id = ? ORDER BY sort, created_at`, bundleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Screenshot{}
	for rows.Next() {
		s, err := scanScreenshot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

// getScreenshot returns one screenshot by id, or (nil, nil) when absent.
func getScreenshot(db *sql.DB, id string) (*Screenshot, error) {
	s, err := scanScreenshot(db.QueryRow(
		`SELECT `+screenshotCols+` FROM app_screenshots WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s, nil
}

// addScreenshot inserts a screenshot, appending it to the end of the app's
// display order.
func addScreenshot(db *sql.DB, s *Screenshot) error {
	if s.CreatedAt == "" {
		s.CreatedAt = store.NowRFC3339()
	}
	var maxSort sql.NullInt64
	if err := db.QueryRow(`SELECT MAX(sort) FROM app_screenshots WHERE bundle_id = ?`,
		s.BundleID).Scan(&maxSort); err != nil {
		return err
	}
	s.Sort = int(maxSort.Int64) + 1
	_, err := db.Exec(`INSERT INTO app_screenshots (`+screenshotCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.BundleID, s.CID, s.Ext, s.Mime, s.Width, s.Height,
		s.Caption, s.Sort, s.CreatedAt)
	return err
}

// screenshotsWithCID counts how many screenshot rows reference a content
// address — so a shared image file is dropped only when the last row using it
// is deleted.
func screenshotsWithCID(db *sql.DB, cid string) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM app_screenshots WHERE cid = ?`, cid).Scan(&n)
	return n, err
}

// setScreenshotCaption updates a screenshot's caption.
func setScreenshotCaption(db *sql.DB, id, caption string) error {
	_, err := db.Exec(`UPDATE app_screenshots SET caption = ? WHERE id = ?`, caption, id)
	return err
}

// deleteScreenshot removes a screenshot row and returns it (for file cleanup),
// or nil when absent.
func deleteScreenshot(db *sql.DB, id string) (*Screenshot, error) {
	s, err := getScreenshot(db, id)
	if err != nil || s == nil {
		return nil, err
	}
	if _, err := db.Exec(`DELETE FROM app_screenshots WHERE id = ?`, id); err != nil {
		return nil, err
	}
	return s, nil
}

// moveScreenshot swaps a screenshot's sort with its neighbour in the given
// direction ("up" or "down"), reordering the gallery.
func moveScreenshot(db *sql.DB, id, dir string) error {
	cur, err := getScreenshot(db, id)
	if err != nil || cur == nil {
		return err
	}
	cmp, order := ">", "ASC"
	if dir == "up" {
		cmp, order = "<", "DESC"
	}
	var nID string
	var nSort int
	err = db.QueryRow(`SELECT id, sort FROM app_screenshots
		WHERE bundle_id = ? AND (sort `+cmp+` ? OR (sort = ? AND id `+cmp+` ?))
		ORDER BY sort `+order+`, id `+order+` LIMIT 1`,
		cur.BundleID, cur.Sort, cur.Sort, cur.ID).Scan(&nID, &nSort)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // already at the edge
	}
	if err != nil {
		return err
	}
	if _, err := db.Exec(`UPDATE app_screenshots SET sort = ? WHERE id = ?`, nSort, cur.ID); err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE app_screenshots SET sort = ? WHERE id = ?`, cur.Sort, nID)
	return err
}

// ── install tokens ───────────────────────────────────────────────────────────

const tokenCols = `token, build_id, kind, state, max_installs, used_installs,
	expires_at, label, last_ua, last_ip, created_at, consumed_at`

func scanToken(row scanner) (*Token, error) {
	var t Token
	if err := row.Scan(&t.Token, &t.BuildID, &t.Kind, &t.State, &t.MaxInstalls,
		&t.UsedInstalls, &t.ExpiresAt, &t.Label, &t.LastUA, &t.LastIP,
		&t.CreatedAt, &t.ConsumedAt); err != nil {
		return nil, err
	}
	return &t, nil
}

func getToken(db *sql.DB, token string) (*Token, error) {
	t, err := scanToken(db.QueryRow(
		`SELECT `+tokenCols+` FROM install_tokens WHERE token = ?`, token))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

// insertToken writes a new token row.
func insertToken(db *sql.DB, t *Token) error {
	if t.CreatedAt == "" {
		t.CreatedAt = store.NowRFC3339()
	}
	_, err := db.Exec(`INSERT INTO install_tokens (`+tokenCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.Token, t.BuildID, t.Kind, t.State, t.MaxInstalls, t.UsedInstalls,
		t.ExpiresAt, t.Label, t.LastUA, t.LastIP, t.CreatedAt, t.ConsumedAt)
	return err
}

// selfToken returns the build's reusable self token, minting one on first use.
// Self tokens are unlimited and never expire — the install page that embeds
// them is session-gated, so the capability URL is only ever seen by the author.
func selfToken(db *sql.DB, buildID string) (*Token, error) {
	t, err := scanToken(db.QueryRow(`SELECT `+tokenCols+` FROM install_tokens
		WHERE build_id = ? AND kind = ? AND state = ?
		ORDER BY created_at DESC LIMIT 1`, buildID, kindSelf, stateActive))
	if err == nil {
		return t, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	t = &Token{
		Token:   auth.NewSessionToken(),
		BuildID: buildID,
		Kind:    kindSelf,
		State:   stateActive,
	}
	if err := insertToken(db, t); err != nil {
		return nil, err
	}
	return t, nil
}

// createShare mints a single-use (or bounded) public share token.
func createShare(db *sql.DB, buildID string, ttl time.Duration, maxInstalls int, label string) (*Token, error) {
	expires := ""
	if ttl > 0 {
		expires = time.Now().UTC().Add(ttl).Format(time.RFC3339)
	}
	t := &Token{
		Token:       auth.NewSessionToken(),
		BuildID:     buildID,
		Kind:        kindShare,
		State:       stateActive,
		MaxInstalls: maxInstalls,
		ExpiresAt:   expires,
		Label:       label,
	}
	if err := insertToken(db, t); err != nil {
		return nil, err
	}
	return t, nil
}

// recordInstall counts one delivered install against a token and flips it to
// consumed when its budget is spent. Self tokens (max 0) count forever.
func recordInstall(db *sql.DB, t *Token, ua, ip string) error {
	t.UsedInstalls++
	t.LastUA, t.LastIP = ua, ip
	consumedAt := t.ConsumedAt
	if t.MaxInstalls > 0 && t.UsedInstalls >= t.MaxInstalls {
		t.State = stateConsumed
		consumedAt = store.NowRFC3339()
		t.ConsumedAt = consumedAt
	}
	_, err := db.Exec(`UPDATE install_tokens
		SET used_installs = ?, last_ua = ?, last_ip = ?, state = ?, consumed_at = ?
		WHERE token = ?`,
		t.UsedInstalls, ua, ip, t.State, consumedAt, t.Token)
	return err
}

// revokeToken marks a token revoked. Reports whether it existed and was active.
func revokeToken(db *sql.DB, token string) (bool, error) {
	res, err := db.Exec(`UPDATE install_tokens SET state = ?
		WHERE token = ? AND state = ?`, stateRevoked, token, stateActive)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// shareRow is a share token joined to its build, for the management table.
type shareRow struct {
	Token
	AppName string
	Version string
}

// listShares returns every share token with its build, newest first.
func listShares(db *sql.DB) ([]shareRow, error) {
	rows, err := db.Query(`SELECT `+tokenColsT("t")+`,
		COALESCE(b.app_name, ''), COALESCE(b.version, '')
		FROM install_tokens t LEFT JOIN builds b ON b.id = t.build_id
		WHERE t.kind = ? ORDER BY t.created_at DESC`, kindShare)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []shareRow{}
	for rows.Next() {
		var s shareRow
		if err := rows.Scan(&s.Token.Token, &s.BuildID, &s.Kind, &s.State,
			&s.MaxInstalls, &s.UsedInstalls, &s.ExpiresAt, &s.Label, &s.LastUA,
			&s.LastIP, &s.CreatedAt, &s.ConsumedAt, &s.AppName, &s.Version); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// tokenColsT qualifies tokenCols with a table alias for joins.
func tokenColsT(alias string) string {
	parts := strings.Split(tokenCols, ",")
	for i, p := range parts {
		parts[i] = alias + "." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}

// pruneTokens drops share tokens that are revoked, fully consumed past the grace
// window, or expired — housekeeping so the table does not accumulate dead rows.
func pruneTokens(db *sql.DB) (int64, error) {
	now := store.NowRFC3339()
	graceCut := time.Now().UTC().Add(-consumeGrace).Format(time.RFC3339)
	res, err := db.Exec(`DELETE FROM install_tokens
		WHERE kind = ? AND (
			state = ? OR
			(state = ? AND consumed_at != '' AND consumed_at <= ?) OR
			(expires_at != '' AND expires_at <= ?)
		)`, kindShare, stateRevoked, stateConsumed, graceCut, now)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
