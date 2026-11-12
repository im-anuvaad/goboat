package edge

// Cloudflare cleanup (docs/architecture.md §5.6): every object goboat
// created at Cloudflare is either in use or queued in cf_cleanup until
// Cloudflare confirms it is gone, and every sync compares the zones goboat
// serves with what Cloudflare lists, so nothing piles up when an API call
// fails, goboat restarts half-way, or an earlier install left things behind.

import (
	"context"
	"crypto/x509"
	"database/sql"
	"encoding/pem"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/im-anuvaad/goboat/internal/cloudflare"
)

// Cleanup kinds.
const (
	cleanHost    = "host"     // DNS records pointing here + Origin CA certs of a hostname no longer served
	cleanAOPCert = "aop_cert" // a zone-level AOP certificate no longer used
	cleanZone    = "zone"     // a zone without goboat hostnames: AOP certs deleted, zone AOP off
)

// CleanupItem is one pending cleanup, for the settings page.
type CleanupItem struct {
	ID        int64     `json:"id"`
	Kind      string    `json:"kind"`
	ZoneID    string    `json:"zone_id"`
	Host      string    `json:"host"`
	ObjectID  string    `json:"object_id"`
	Reason    string    `json:"reason"`
	Attempts  int       `json:"attempts"`
	LastError string    `json:"last_error"`
	CreatedAt time.Time `json:"created_at"`
	LastTry   time.Time `json:"last_try"`
}

// What says what the item removes, in words.
func (c CleanupItem) What() string {
	switch c.Kind {
	case cleanHost:
		return "DNS records and Origin CA certificates of " + c.Host
	case cleanAOPCert:
		return "origin pull certificate " + short(c.ObjectID)
	case cleanZone:
		return "origin pull setup of a zone goboat no longer uses"
	}
	return c.Kind
}

// Failing reports whether the item has failed for longer than d.
func (c CleanupItem) Failing(d time.Duration) bool {
	return c.LastError != "" && time.Since(c.CreatedAt) > d
}

// UnusedCert is an Origin CA certificate in a zone goboat serves whose
// hostnames goboat doesn't serve. goboat never revokes these by itself:
// they may belong to another origin.
type UnusedCert struct {
	ID        string   `json:"id"`
	Zone      string   `json:"zone"`
	Hostnames []string `json:"hostnames"`
	ExpiresOn string   `json:"expires_on"`
}

func short(id string) string {
	if len(id) > 10 {
		return id[:10] + "…"
	}
	return id
}

type execer interface {
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
}

// enqueue adds a cleanup item (a duplicate is ignored).
func enqueue(ctx context.Context, x execer, kind, zoneID, host, objectID, detail, reason string) error {
	_, err := x.ExecContext(ctx, `INSERT INTO cf_cleanup (kind, zone_id, host, object_id, detail, reason, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`, kind, zoneID, host, objectID, detail, reason, time.Now().Unix())
	return err
}

// Cleanup lists the pending cleanup items, oldest first.
func (m *Manager) Cleanup(ctx context.Context) []CleanupItem {
	rows, err := m.store.Reader().QueryContext(ctx, `SELECT id, kind, zone_id, host, object_id, reason, attempts, last_error,
		created_at, last_try FROM cf_cleanup ORDER BY id`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []CleanupItem
	for rows.Next() {
		var c CleanupItem
		var created, tried int64
		if rows.Scan(&c.ID, &c.Kind, &c.ZoneID, &c.Host, &c.ObjectID, &c.Reason, &c.Attempts, &c.LastError, &created, &tried) == nil {
			c.CreatedAt = time.Unix(created, 0)
			if tried > 0 {
				c.LastTry = time.Unix(tried, 0)
			}
			out = append(out, c)
		}
	}
	return out
}

type cleanupRow struct {
	CleanupItem
	detail string
}

func (m *Manager) pending(ctx context.Context, kinds ...string) ([]cleanupRow, error) {
	q := `SELECT id, kind, zone_id, host, object_id, detail, attempts FROM cf_cleanup WHERE kind IN (?` +
		strings.Repeat(", ?", len(kinds)-1) + `) ORDER BY id`
	args := make([]any, len(kinds))
	for i, k := range kinds {
		args[i] = k
	}
	rows, err := m.store.Reader().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []cleanupRow
	for rows.Next() {
		var r cleanupRow
		if err := rows.Scan(&r.ID, &r.Kind, &r.ZoneID, &r.Host, &r.ObjectID, &r.detail, &r.Attempts); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (m *Manager) cleanupDone(id int64, what string) {
	if _, err := m.store.Writer().Exec(`DELETE FROM cf_cleanup WHERE id = ?`, id); err != nil {
		m.log.Warn("cleanup bookkeeping", "err", err)
		return
	}
	if what != "" {
		m.log.Info("cleaned up at Cloudflare", "what", what)
	}
}

func (m *Manager) cleanupFailed(id int64, err error) {
	if _, e := m.store.Writer().Exec(`UPDATE cf_cleanup SET attempts = attempts + 1, last_error = ?, last_try = ? WHERE id = ?`,
		err.Error(), time.Now().Unix(), id); e != nil {
		m.log.Warn("cleanup bookkeeping", "err", e)
	}
}

// knownHosts are the hostnames goboat is configured to serve, read from the
// database (every app's domain and the dashboard domain), so an app that
// failed to load never has its domain cleaned up. ok=false when the list
// can't be trusted; nothing is cleaned up for hosts then.
func (m *Manager) knownHosts(ctx context.Context, served []hostEntry) (map[string]bool, bool) {
	known := map[string]bool{}
	for _, h := range served {
		known[h.Host] = true
	}
	if m.cfg.KnownHosts == nil {
		return known, true
	}
	list, err := m.cfg.KnownHosts(ctx)
	if err != nil {
		m.log.Warn("cannot list the configured domains; skipping Cloudflare cleanup this time", "err", err)
		return known, false
	}
	for _, h := range list {
		if h != "" {
			known[normalizeHost(h)] = true
		}
	}
	return known, true
}

// RemoveHost stops managing a hostname (app deleted, domain changed,
// dashboard domain changed): its Cloudflare objects are queued for cleanup
// in one transaction with forgetting its certificate, the local copy is
// deleted, and a background sync does the work. It doesn't call Cloudflare,
// so it never waits on it or on a running sync.
func (m *Manager) RemoveHost(ctx context.Context, host string) error {
	host = normalizeHost(host)
	m.mu.Lock()
	zoneID := m.hostZone[host]
	delete(m.hostZone, host)
	delete(m.hostState, host)
	addrs := strings.Trim(m.pubV4+","+strings.Replace(m.pubV6, "off", "", 1), ",")
	m.mu.Unlock()

	tx, err := m.store.Writer().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var rowZone string
	if err := tx.QueryRowContext(ctx, `SELECT zone_id FROM edge_certs WHERE hostname = ?`, host).Scan(&rowZone); err == nil && rowZone != "" {
		zoneID = rowZone
	}
	if err := enqueue(ctx, tx, cleanHost, zoneID, host, "", addrs, "no longer served"); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM edge_certs WHERE hostname = ?`, host); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	m.Certs.Delete(host)
	m.log.Info("hostname no longer served; its Cloudflare objects are queued for cleanup", "host", host)
	m.SyncInBackground()
	return nil
}

// originCache lists each zone's Origin CA certificates once per sync.
type originCache struct {
	c     *cloudflare.Client
	zones map[string][]cloudflare.OriginCert
}

func (o *originCache) list(ctx context.Context, zoneID string) ([]cloudflare.OriginCert, error) {
	if cs, ok := o.zones[zoneID]; ok {
		return cs, nil
	}
	cs, err := o.c.ListOriginCerts(ctx, zoneID)
	if err != nil {
		return nil, err
	}
	o.zones[zoneID] = cs
	return cs, nil
}

// forget drops a revoked certificate from the cached list. It builds a new
// slice: callers may be ranging over the old one.
func (o *originCache) forget(zoneID, id string) {
	var keep []cloudflare.OriginCert
	for _, c := range o.zones[zoneID] {
		if c.ID != id {
			keep = append(keep, c)
		}
	}
	o.zones[zoneID] = keep
}

// cleanHosts works through the queued host and AOP certificate items.
func (m *Manager) cleanHosts(ctx context.Context, c *cloudflare.Client, oc *originCache, known map[string]bool, v4, v6 string) error {
	items, err := m.pending(ctx, cleanHost, cleanAOPCert)
	if err != nil {
		return err
	}
	var errs []error
	for _, it := range items {
		var err error
		switch it.Kind {
		case cleanHost:
			if known[it.Host] {
				// Served again: its records are updated in place and a
				// duplicate certificate is revoked by reconcileZone.
				m.cleanupDone(it.ID, "")
				continue
			}
			err = m.cleanHost(ctx, c, oc, it, v4, v6)
		case cleanAOPCert:
			err = m.cleanAOPCert(ctx, c, it)
		}
		if err != nil {
			m.cleanupFailed(it.ID, err)
			errs = append(errs, fmt.Errorf("cleanup of %s: %w", it.What(), err))
			continue
		}
		m.cleanupDone(it.ID, it.What())
	}
	return errors.Join(errs...)
}

func (m *Manager) cleanHost(ctx context.Context, c *cloudflare.Client, oc *originCache, it cleanupRow, v4, v6 string) error {
	zoneID := it.ZoneID
	if zoneID == "" {
		z, err := c.ZoneFor(ctx, it.Host)
		if errors.Is(err, cloudflare.ErrNoZone) {
			return nil // not in this account (any more): nothing of ours can be there
		}
		if err != nil {
			return err
		}
		zoneID = z.ID
	}
	// Only records that still point at this server are ours to delete: after
	// a server move they point at the new server and are left alone.
	ours := map[string]bool{}
	for _, a := range append(strings.Split(it.detail, ","), v4, v6) {
		if a != "" {
			ours[a] = true
		}
	}
	rs, err := c.DNSRecords(ctx, zoneID, it.Host)
	if err != nil {
		return fmt.Errorf("DNS: %w", err)
	}
	for _, r := range rs {
		if (r.Type == "A" || r.Type == "AAAA") && ours[r.Content] {
			if err := c.DeleteDNSRecord(ctx, zoneID, r.ID); err != nil && !cloudflare.IsNotFound(err) {
				return fmt.Errorf("DNS: %w", err)
			}
		}
	}
	certs, err := oc.list(ctx, zoneID)
	if err != nil {
		return fmt.Errorf("listing certificates: %w", err)
	}
	for _, oc2 := range certs {
		if len(oc2.Hostnames) == 1 && oc2.Hostnames[0] == it.Host {
			if err := c.RevokeOriginCert(ctx, oc2.ID); err != nil && !cloudflare.IsNotFound(err) {
				return fmt.Errorf("revoke certificate: %w", err)
			}
			oc.forget(zoneID, oc2.ID)
		}
	}
	return nil
}

func (m *Manager) cleanAOPCert(ctx context.Context, c *cloudflare.Client, it cleanupRow) error {
	m.mu.RLock()
	zs := m.zoneState[it.ZoneID]
	cur := ""
	if zs != nil {
		cur = zs.AOPCertID
	}
	m.mu.RUnlock()
	if it.ObjectID == cur {
		return nil // in use again
	}
	// Never take a certificate away before its replacement is active.
	if cur != "" {
		if st, err := c.AOPCert(ctx, it.ZoneID, cur); err != nil {
			return err
		} else if st.Status != "active" {
			return fmt.Errorf("waiting for the current origin pull certificate to become active (it is %s)", st.Status)
		}
	}
	if err := c.DeleteAOPCert(ctx, it.ZoneID, it.ObjectID); err != nil && !cloudflare.IsNotFound(err) {
		return err
	}
	return nil
}

// reconcileZone compares a zone goboat serves with Cloudflare's lists:
// Origin CA certificates for a served hostname other than the one goboat
// serves are revoked; certificates for other hostnames are reported as
// unused; zone-level AOP certificates other than goboat's current one are
// deleted once it is active (aopReady).
func (m *Manager) reconcileZone(ctx context.Context, c *cloudflare.Client, oc *originCache, z cloudflare.Zone, known map[string]bool, aopReady bool) ([]UnusedCert, error) {
	var errs []error
	var unused []UnusedCert
	certs, err := oc.list(ctx, z.ID)
	if err != nil {
		return nil, fmt.Errorf("listing certificates: %w", err)
	}
	for _, cert := range certs {
		served := ""
		for _, h := range cert.Hostnames {
			if known[h] {
				served = h
			}
		}
		if served == "" {
			unused = append(unused, UnusedCert{ID: cert.ID, Zone: z.Name, Hostnames: cert.Hostnames, ExpiresOn: cert.ExpiresOn})
			continue
		}
		if len(cert.Hostnames) != 1 {
			continue // not one of ours (goboat's certificates name one hostname)
		}
		leaf := m.Certs.Leaf(served)
		if leaf == nil {
			continue // not installed here yet: can't tell which one is current
		}
		if sameCert(cert.Certificate, leaf) {
			continue
		}
		if err := c.RevokeOriginCert(ctx, cert.ID); err != nil && !cloudflare.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("revoking an extra certificate for %s: %w", served, err))
			continue
		}
		oc.forget(z.ID, cert.ID)
		m.log.Info("revoked an extra Origin CA certificate", "host", served, "id", cert.ID)
	}

	m.mu.RLock()
	cur := ""
	if zs := m.zoneState[z.ID]; zs != nil {
		cur = zs.AOPCertID
	}
	m.mu.RUnlock()
	if m.cfg.AOP && aopReady && cur != "" {
		list, err := c.ListAOPCerts(ctx, z.ID)
		if err != nil {
			errs = append(errs, fmt.Errorf("listing origin pull certificates: %w", err))
		}
		for _, a := range list {
			if a.ID == cur || a.Status == "deleted" || a.Status == "pending_deletion" {
				continue
			}
			if err := c.DeleteAOPCert(ctx, z.ID, a.ID); err != nil && !cloudflare.IsNotFound(err) {
				enqueue(ctx, m.store.Writer(), cleanAOPCert, z.ID, "", a.ID, "", "not goboat's current certificate")
				errs = append(errs, fmt.Errorf("deleting an extra origin pull certificate: %w", err))
				continue
			}
			m.log.Info("deleted an extra origin pull certificate", "zone", z.Name, "id", a.ID)
		}
	}
	sort.Slice(unused, func(i, j int) bool {
		return strings.Join(unused[i].Hostnames, ",") < strings.Join(unused[j].Hostnames, ",")
	})
	return unused, errors.Join(errs...)
}

// sameCert reports whether certPEM is the certificate leaf.
func sameCert(certPEM string, leaf *x509.Certificate) bool {
	b, _ := pem.Decode([]byte(certPEM))
	if b == nil {
		return false
	}
	c, err := x509.ParseCertificate(b.Bytes)
	return err == nil && c.SerialNumber.Cmp(leaf.SerialNumber) == 0
}

// forgetOrphans queues the cleanup of hostnames that have a certificate
// record but aren't configured any more (a sync that raced with a removal,
// or a removal by an older goboat), and deletes certificate files nobody
// uses.
func (m *Manager) forgetOrphans(ctx context.Context, known map[string]bool) error {
	rows, err := m.store.Reader().QueryContext(ctx, `SELECT hostname, zone_id FROM edge_certs`)
	if err != nil {
		return err
	}
	type orphan struct{ host, zone string }
	var os []orphan
	for rows.Next() {
		var o orphan
		if rows.Scan(&o.host, &o.zone) == nil && !known[o.host] {
			os = append(os, o)
		}
	}
	rows.Close()
	for _, o := range os {
		if err := enqueue(ctx, m.store.Writer(), cleanHost, o.zone, o.host, "", "", "not configured any more"); err != nil {
			return err
		}
		if _, err := m.store.Writer().ExecContext(ctx, `DELETE FROM edge_certs WHERE hostname = ?`, o.host); err != nil {
			return err
		}
		m.log.Info("queued the cleanup of a hostname that is not configured", "host", o.host)
	}
	for _, h := range m.Certs.Hosts() {
		if !known[h] {
			m.Certs.Delete(h)
		}
	}
	return nil
}

// releaseZones queues zones that no configured hostname uses any more and
// works through the queued zone items: goboat's AOP certificates there are
// deleted and zone-level AOP turned off. servedZones must be complete (every
// configured hostname's zone known), or nothing is released.
func (m *Manager) releaseZones(ctx context.Context, c *cloudflare.Client, servedZones map[string]bool) error {
	m.mu.RLock()
	var stale []string
	for id := range m.zoneState {
		if !servedZones[id] {
			stale = append(stale, id)
		}
	}
	m.mu.RUnlock()
	for _, id := range stale {
		if err := enqueue(ctx, m.store.Writer(), cleanZone, id, "", "", "", "no goboat hostnames left in the zone"); err != nil {
			return err
		}
	}
	items, err := m.pending(ctx, cleanZone)
	if err != nil {
		return err
	}
	var errs []error
	for _, it := range items {
		if servedZones[it.ZoneID] {
			m.cleanupDone(it.ID, "")
			continue
		}
		if err := m.releaseZone(ctx, c, it.ZoneID); err != nil {
			m.cleanupFailed(it.ID, err)
			errs = append(errs, fmt.Errorf("releasing zone %s: %w", it.ZoneID, err))
			continue
		}
		m.cleanupDone(it.ID, "origin pull setup of zone "+it.ZoneID)
	}
	return errors.Join(errs...)
}

func (m *Manager) releaseZone(ctx context.Context, c *cloudflare.Client, zoneID string) error {
	if m.cfg.AOP {
		list, err := c.ListAOPCerts(ctx, zoneID)
		if err != nil && !cloudflare.IsNotFound(err) {
			return err
		}
		if on, err := c.AOPEnabled(ctx, zoneID); err != nil && !cloudflare.IsNotFound(err) {
			return err
		} else if on {
			if err := c.SetAOPEnabled(ctx, zoneID, false); err != nil {
				return err
			}
		}
		for _, a := range list {
			if a.Status == "deleted" || a.Status == "pending_deletion" {
				continue
			}
			if err := c.DeleteAOPCert(ctx, zoneID, a.ID); err != nil && !cloudflare.IsNotFound(err) {
				return err
			}
		}
	}
	if _, err := m.store.Writer().ExecContext(ctx, `DELETE FROM edge_zones WHERE zone_id = ?`, zoneID); err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.zoneState, zoneID)
	delete(m.enforce, zoneID)
	delete(m.rollout, zoneID)
	if t := m.rollTimer[zoneID]; t != nil {
		t.Stop()
		delete(m.rollTimer, zoneID)
	}
	m.mu.Unlock()
	return nil
}

// Unused lists the Origin CA certificates for hostnames goboat doesn't
// serve, as of the last sync.
func (m *Manager) Unused() []UnusedCert {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]UnusedCert(nil), m.unused...)
}

// RevokeUnused revokes one certificate from the Unused list (the user's
// decision, from the settings page).
func (m *Manager) RevokeUnused(ctx context.Context, id string) error {
	m.mu.RLock()
	var found *UnusedCert
	for i := range m.unused {
		if m.unused[i].ID == id {
			found = &m.unused[i]
		}
	}
	m.mu.RUnlock()
	if found == nil {
		return errors.New("that certificate is not in the list of unused certificates (press Sync now to refresh it)")
	}
	hosts := strings.Join(found.Hostnames, ", ")
	token, err := m.token(ctx)
	if err != nil {
		return err
	}
	if err := m.client(token).RevokeOriginCert(ctx, id); err != nil && !cloudflare.IsNotFound(err) {
		return err
	}
	m.mu.Lock()
	m.unused = slices.DeleteFunc(m.unused, func(u UnusedCert) bool { return u.ID == id })
	m.mu.Unlock()
	m.log.Info("revoked an unused Origin CA certificate", "hostnames", hosts, "id", id)
	return nil
}
