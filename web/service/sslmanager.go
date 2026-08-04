package service

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mhsanaei/3x-ui/v2/logger"
)

// SSLService is the entry point for everything certificate-related: the store,
// the ledger, the preflight, the acme.sh driver and the consumer fan-out.
//
// Zero-value usable and stateless, like the other services in this package, so a
// fresh copy works and callers do not have to thread a constructor through. The
// only shared mutable state is the background run below, which is package-level
// for the same reason provisionRun is (core.go:1437): issuance is single-admin and
// one-at-a-time by design, not by accident.
type SSLService struct{}

// ---------------------------------------------------------------------------
// Status
// ---------------------------------------------------------------------------

// SSLStatus is one call that answers everything the settings page needs to render.
type SSLStatus struct {
	StoreRoot string `json:"storeRoot"`
	AcmeHome  string `json:"acmeHome"`

	// CertPath and KeyPath are the STABLE paths to put in webCertFile/webKeyFile.
	// They stay valid across every renewal and every switch, which is the whole
	// reason the store exists.
	CertPath string `json:"certPath"`
	KeyPath  string `json:"keyPath"`

	// Active describes what those paths currently resolve to, or nil when nothing
	// is managed yet.
	Active *SSLCertInfo `json:"active,omitempty"`

	// UsedByPanel reports whether the panel's own listener is pointed at the
	// managed path. False means a renewal will change nothing for the panel.
	UsedByPanel bool          `json:"usedByPanel"`
	Consumers   []SSLConsumer `json:"consumers"`

	// Running is the in-flight operation, if any. While it is non-nil the UI
	// should show progress rather than an enabled button: a second request is
	// refused, not queued.
	Running *SSLRunningOp `json:"running,omitempty"`

	Budget   SSLBudget    `json:"budget"`
	Attempts []SSLAttempt `json:"attempts"`

	// Versions are the stored versions of the ACTIVE identifier set, newest
	// first, for rollback.
	Versions []string `json:"versions"`
}

// SSLRunningOp is the in-flight operation, for the "already running" case.
type SSLRunningOp struct {
	Op          string    `json:"op"`
	Identifiers []string  `json:"identifiers"`
	StartedAt   time.Time `json:"startedAt"`
	PID         int       `json:"pid"`
}

// Status gathers the full picture. Errors from the individual probes are folded
// into the result rather than returned, because a settings page that renders
// nothing because one inbound has malformed JSON is worse than one that renders
// most of the truth.
func (s *SSLService) Status() (*SSLStatus, error) {
	root := DefaultSSLStoreRoot()
	store, err := OpenSSLStore(root)
	if err != nil {
		return nil, err
	}
	st := &SSLStatus{
		StoreRoot: root,
		AcmeHome:  SSLAcmeHome(root),
		CertPath:  store.ActiveCertPath(),
		KeyPath:   store.ActiveKeyPath(),
	}

	if store.HasActive() {
		if info, err := store.ActiveInfo(); err == nil {
			st.Active = info
			st.Versions = store.Versions(SSLIdentifierSetKey(info.Identifiers))
		} else {
			logger.Warning("ssl: active certificate could not be parsed:", err)
		}
	}

	var ss SettingService
	if p, err := ss.GetCertFile(); err == nil {
		st.UsedByPanel = filepath.Clean(p) == filepath.Clean(st.CertPath)
	}
	if consumers, err := ListSSLConsumers(st.CertPath); err == nil {
		st.Consumers = consumers
	} else {
		logger.Warning("ssl: could not list certificate consumers:", err)
	}

	if rec, ok := SSLIssuanceRunning(root); ok {
		st.Running = &rec
	}

	if ledger, err := OpenSSLLedger(SSLLedgerPath(root)); err == nil {
		st.Attempts = ledger.Attempts()
		if st.Active != nil {
			st.Budget = ledger.Budget(SSLIdentifierSetKey(st.Active.Identifiers), sslCAProduction)
		}
	} else {
		logger.Warning("ssl: ledger unavailable:", err)
	}
	return st, nil
}

// Preflight runs every local check and returns the verdict WITHOUT contacting any
// CA. Safe to call as often as the UI likes; that is the point of it.
func (s *SSLService) Preflight(req SSLPreflightRequest) (SSLPreflightResult, error) {
	root := DefaultSSLStoreRoot()
	store, err := OpenSSLStore(root)
	if err != nil {
		return SSLPreflightResult{}, err
	}
	ledger, err := OpenSSLLedger(SSLLedgerPath(root))
	if err != nil {
		return SSLPreflightResult{}, err
	}
	var active *SSLCertInfo
	if store.HasActive() {
		active, _ = store.ActiveInfo()
	}
	return sslRunPreflight(req, active, ledger, defaultSSLPreflightDeps(root)), nil
}

// Consumers lists everything pointed at the managed certificate path, so the UI
// can show which protocols would drop connections BEFORE the operator agrees to a
// disruptive apply.
func (s *SSLService) Consumers() ([]SSLConsumer, error) {
	root := DefaultSSLStoreRoot()
	store, err := OpenSSLStore(root)
	if err != nil {
		return nil, err
	}
	return ListSSLConsumers(store.ActiveCertPath())
}

// UseManagedCertificate points the panel and subscription listeners at the store's
// stable paths. Idempotent, and worth doing exactly once: after this, no renewal
// ever needs a setting changed again.
//
// Refuses when nothing is active, because writing paths that do not resolve is
// precisely how web.go:541-556 ends up silently serving plain HTTP after the next
// restart.
func (s *SSLService) UseManagedCertificate() error {
	root := DefaultSSLStoreRoot()
	store, err := OpenSSLStore(root)
	if err != nil {
		return err
	}
	if _, err := sslValidatePair(store.ActiveCertPath(), store.ActiveKeyPath()); err != nil {
		return fmt.Errorf("the managed certificate is not usable yet (%w). Issue one first", err)
	}
	var ss SettingService
	if err := ss.SetCertFile(store.ActiveCertPath()); err != nil {
		return err
	}
	if err := ss.SetKeyFile(store.ActiveKeyPath()); err != nil {
		return err
	}
	if err := ss.SetSubCertFile(store.ActiveCertPath()); err != nil {
		return err
	}
	return ss.SetSubKeyFile(store.ActiveKeyPath())
}

// Rollback re-points the active link at a stored version. The version has to
// revalidate, so a rollback cannot be the thing that breaks TLS either.
func (s *SSLService) Rollback(version string) error {
	root := DefaultSSLStoreRoot()
	store, err := OpenSSLStore(root)
	if err != nil {
		return err
	}
	// Refuse a path outside the store rather than symlinking wherever we are
	// pointed: this is reachable from an HTTP handler.
	rel, err := filepath.Rel(root, filepath.Clean(version))
	if err != nil || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("%q is not a version in this store", version)
	}
	if err := store.Activate(filepath.Clean(version)); err != nil {
		return err
	}
	ApplySSLConsumers(store.ActiveCertPath(), SSLFanOutOptions{}, func(ProvisionStep) {})
	return nil
}

// ---------------------------------------------------------------------------
// The background run
// ---------------------------------------------------------------------------

// SSLOperationRequest is one operator action.
type SSLOperationRequest struct {
	SSLIssueRequest
	FanOut SSLFanOutOptions `json:"fanOut"`
}

// sslRun holds the single in-progress or most-recent run, so the settings page can
// poll a live log. Same shape as provisionRun (core.go:1437-1445) so the existing
// setup-console component renders it without changes.
var sslRun struct {
	mu      sync.Mutex
	running bool
	done    bool
	op      string
	steps   []ProvisionStep
	failed  bool
	summary string
}

// SSLRunState is a snapshot of the background run.
type SSLRunState struct {
	Running bool            `json:"running"`
	Done    bool            `json:"done"`
	Op      string          `json:"op"`
	Steps   []ProvisionStep `json:"steps"`
	Failed  bool            `json:"failed"`
	Summary string          `json:"summary"`
}

// RunState returns the current or most-recent run's progress.
func (s *SSLService) RunState() SSLRunState {
	sslRun.mu.Lock()
	defer sslRun.mu.Unlock()
	steps := make([]ProvisionStep, len(sslRun.steps))
	copy(steps, sslRun.steps)
	return SSLRunState{
		Running: sslRun.running, Done: sslRun.done, Op: sslRun.op,
		Steps: steps, Failed: sslRun.failed, Summary: sslRun.summary,
	}
}

// Start launches an operation in the background, or refuses.
//
// REFUSES rather than queues, and the error says since when the other run has been
// going. Every operation here either costs metered CA budget or restarts a daemon,
// so a second one the operator did not knowingly start is never the right answer.
func (s *SSLService) Start(req SSLOperationRequest) error {
	root := DefaultSSLStoreRoot()
	store, err := OpenSSLStore(root)
	if err != nil {
		return err
	}

	sslRun.mu.Lock()
	if sslRun.running {
		sslRun.mu.Unlock()
		return fmt.Errorf("an SSL operation is already running")
	}
	sslRun.mu.Unlock()

	// The file lock is what a separate CLI invocation of this binary also sees.
	release, err := sslAcquireIssuance(root, req.Op, req.Identifiers, time.Now)
	if err != nil {
		return err
	}

	sslRun.mu.Lock()
	sslRun.running, sslRun.done, sslRun.failed = true, false, false
	sslRun.op, sslRun.steps, sslRun.summary = req.Op, nil, ""
	sslRun.mu.Unlock()

	go func() {
		defer release()
		emit := func(st ProvisionStep) {
			sslRun.mu.Lock()
			sslRun.steps = append(sslRun.steps, st)
			sslRun.mu.Unlock()
		}
		failed, summary := s.run(store, req, emit)
		sslRun.mu.Lock()
		sslRun.running, sslRun.done = false, true
		sslRun.failed, sslRun.summary = failed, summary
		sslRun.mu.Unlock()
	}()
	return nil
}

// run is the operation itself. Ordering is deliberate: everything free happens
// before anything metered, so a run that is going to be refused is refused before
// it can cost anything.
func (s *SSLService) run(store *SSLStore, req SSLOperationRequest, emit func(ProvisionStep)) (failed bool, summary string) {
	root := store.Root()
	setKey := SSLIdentifierSetKey(req.Identifiers)

	ledger, err := OpenSSLLedger(SSLLedgerPath(root))
	if err != nil {
		emit(ProvisionStep{Name: "ledger", Msg: err.Error()})
		return true, err.Error()
	}

	// Re-apply never contacts a CA, so it skips the preflight entirely: refusing
	// it on "the certificate is still valid" would refuse the one operation whose
	// whole purpose is to run when the certificate IS still valid.
	if req.Op == SSLOpReapply {
		return s.runReapply(store, req, emit)
	}

	var active *SSLCertInfo
	if store.HasActive() {
		active, _ = store.ActiveInfo()
	}
	pre := sslRunPreflight(req.PreflightRequest(), active, ledger, defaultSSLPreflightDeps(root))
	for _, st := range pre.Steps {
		emit(st)
	}
	if pre.Blocked {
		return true, pre.Reason
	}

	// Host prerequisites. EnsureAcmeDeps is reused verbatim: acme.sh's own
	// pre-check hard-fails without a cron daemon, and --standalone needs socat or
	// python (acmedeps.go).
	emit(ProvisionStep{Name: "acme.sh dependencies", OK: true, Msg: EnsureAcmeDeps()})

	driver := newSSLAcmeDriver(root)
	if err := driver.EnsureAcmeHome(emit); err != nil {
		emit(ProvisionStep{Name: "acme home", Msg: err.Error()})
		return true, err.Error()
	}

	// Cloudflare credentials are checked BEFORE issuance because a bad token
	// fails several minutes into acme.sh in a way that reads as a DNS problem
	// (cloudflare.go:112).
	var env []string
	if req.Challenge == SSLChallengeCloudflareDNS {
		token := strings.TrimSpace(req.CloudflareToken)
		if _, err := VerifyCloudflareToken(token); err != nil {
			msg := fmt.Sprintf("The Cloudflare API token was rejected: %v. It needs Zone:DNS:Edit on this zone, and Zone:Zone:Read to be listed at all.", err)
			emit(ProvisionStep{Name: "Cloudflare token", Msg: msg})
			return true, msg
		}
		emit(ProvisionStep{Name: "Cloudflare token", OK: true, Msg: "The API token is active."})
		// In the environment, never in argv: /proc/<pid>/cmdline is world-readable.
		env = append(env, "CF_Token="+token)
	}

	// From here on the CA is involved and everything is metered.
	args, err := driver.opArgs(req.SSLIssueRequest)
	if err != nil {
		emit(ProvisionStep{Name: req.Op, Msg: err.Error()})
		return true, err.Error()
	}

	label := "issue a new certificate (spends one of 5 per 7 days for this exact set of names)"
	if req.Op == SSLOpRenew {
		label = "renew (ARI-coordinated, exempt from Let's Encrypt rate limits)"
	}
	emit(ProvisionStep{Name: "contacting Let's Encrypt", OK: true, Msg: label, Log: sslRedactArgs(args)})

	out, code, runErr := driver.exec(args, env)
	chain := driver.issuedChainPath(req.SSLIssueRequest.Primary())

	// acme.sh's RENEW_SKIP means it decided the certificate is not due and never
	// contacted the CA. Not a failure, and recording it as one would poison the
	// backoff for an operation that cost nothing.
	if req.Op == SSLOpRenew && code == sslAcmeRenewSkip {
		emit(ProvisionStep{Name: "renew", OK: true, Warn: true,
			Msg: "acme.sh reports this certificate is not due for renewal yet, so it did not contact Let's Encrypt. Nothing was spent.",
			Log: strings.TrimSpace(out)})
		return false, "Not due for renewal; nothing was changed."
	}

	// THE GATE. On the fullchain FILE, never on the domain directory: acme.sh
	// creates the directory (with a domain key in it) even when validation fails,
	// so its presence proves nothing. vpn-ui.sh:663-681 records what gating on the
	// directory cost.
	if chain == "" {
		msg := sslExplainFailure(req.SSLIssueRequest, out, runErr)
		emit(ProvisionStep{Name: req.Op, Msg: msg, Log: strings.TrimSpace(out)})
		s.record(ledger, req.SSLIssueRequest, setKey, false, msg)
		return true, msg
	}
	s.record(ledger, req.SSLIssueRequest, setKey, true, "")
	emit(ProvisionStep{Name: req.Op, OK: true, Msg: "Let's Encrypt issued the certificate.", Log: strings.TrimSpace(out)})

	if err := s.installStageActivate(store, driver, req, setKey, emit); err != nil {
		return true, err.Error()
	}
	ApplySSLConsumers(store.ActiveCertPath(), req.FanOut, emit)
	return false, "The certificate is active."
}

// runReapply re-installs from the material already on disk and re-runs the fan-out.
// It cannot contact a CA and therefore cannot fail a rate limit, which is why it is
// the primary action: most "the certificate is not working" reports are a consumer
// holding a stale copy, not a certificate that needs reissuing.
func (s *SSLService) runReapply(store *SSLStore, req SSLOperationRequest, emit func(ProvisionStep)) (bool, string) {
	setKey := SSLIdentifierSetKey(req.Identifiers)
	driver := newSSLAcmeDriver(store.Root())

	// Best-effort: when acme.sh has the material, re-installing picks up anything
	// its own cron renewed behind our back. When it does not, the active version
	// is already the truth and only the fan-out is needed.
	if driver.issuedChainPath(req.SSLIssueRequest.Primary()) != "" {
		if err := s.installStageActivate(store, driver, req, setKey, emit); err != nil {
			emit(ProvisionStep{Name: "re-apply", OK: true, Warn: true, Msg: "Could not re-install from acme.sh (" + err.Error() + "). Continuing with the certificate already active."})
		}
	} else {
		emit(ProvisionStep{Name: "re-apply", OK: true, Msg: "No acme.sh material for these names; re-applying the certificate already active."})
	}

	if !store.HasActive() {
		msg := "There is no active managed certificate to re-apply. Issue one first."
		emit(ProvisionStep{Name: "re-apply", Msg: msg})
		return true, msg
	}
	ApplySSLConsumers(store.ActiveCertPath(), req.FanOut, emit)
	return false, "Re-applied the active certificate. No CA was contacted."
}

// installStageActivate is the promotion path: acme.sh writes into the landing
// zone, the store validates and versions it, and only then does the active link
// move.
func (s *SSLService) installStageActivate(store *SSLStore, driver *sslAcmeDriver, req SSLOperationRequest, setKey string, emit func(ProvisionStep)) error {
	_, certPath, keyPath, err := driver.sslInstallPaths(setKey)
	if err != nil {
		emit(ProvisionStep{Name: "install", Msg: err.Error()})
		return err
	}
	args, err := driver.installArgs(req.SSLIssueRequest, certPath, keyPath)
	if err != nil {
		emit(ProvisionStep{Name: "install", Msg: err.Error()})
		return err
	}
	if out, _, err := driver.exec(args, nil); err != nil {
		msg := fmt.Sprintf("acme.sh could not install the certificate: %v", err)
		emit(ProvisionStep{Name: "install", Msg: msg, Log: strings.TrimSpace(out)})
		return fmt.Errorf("%s", msg)
	}
	emit(ProvisionStep{Name: "install", OK: true, Msg: "Wrote the certificate into the managed store's landing zone."})

	version, err := store.StageFromFiles(setKey, certPath, keyPath)
	if err != nil {
		// The invariant held: nothing became active. Say so, because "issuance
		// succeeded but the panel still serves the old certificate" is otherwise
		// indistinguishable from a silent failure.
		msg := fmt.Sprintf("%v. The previously active certificate was left untouched and is still being served.", err)
		emit(ProvisionStep{Name: "validate", Msg: msg})
		return fmt.Errorf("%s", msg)
	}
	emit(ProvisionStep{Name: "validate", OK: true, Msg: "The certificate and key load together and match."})

	if err := store.Activate(version); err != nil {
		msg := fmt.Sprintf("%v. The previously active certificate was left untouched.", err)
		emit(ProvisionStep{Name: "activate", Msg: msg})
		return fmt.Errorf("%s", msg)
	}
	info, _ := store.ActiveInfo()
	detail := "The new certificate is active."
	if info != nil {
		detail = fmt.Sprintf("Active: %s, issued by %s, valid until %s (%s).",
			strings.Join(info.Identifiers, ", "), info.Issuer, sslFormatTime(info.NotAfter), sslFormatDuration(info.Remaining))
		if !info.HasIntermediates {
			// Worth its own warning: the panel works either way in a browser, but
			// stock Windows (the SSTP and IKEv2 audience) will not fetch a missing
			// issuer and fails with a message that mentions credentials, not chains.
			detail += " NOTE: the file contains no intermediate certificate, which stock Windows clients (SSTP, IKEv2) cannot work around."
		}
	}
	emit(ProvisionStep{Name: "activate", OK: true, Warn: info != nil && !info.HasIntermediates, Msg: detail})
	return nil
}

func (s *SSLService) record(ledger *SSLLedger, req SSLIssueRequest, setKey string, success bool, msg string) {
	if err := ledger.Record(SSLAttempt{
		Identifiers: req.Identifiers,
		SetKey:      setKey,
		CA:          req.CA(),
		Op:          req.Op,
		// Only the --renew path carries the RFC 9773 "replaces" field and is
		// therefore exempt from every Let's Encrypt limit. See sslacme.go.
		Exempt:  req.Op == SSLOpRenew,
		Success: success,
		Message: msg,
	}); err != nil {
		// A ledger we cannot write is a ledger that will under-count next time,
		// which is the permissive direction. Loud, because it matters.
		logger.Warning("ssl: FAILED to record an issuance attempt, the local rate-limit guard is now under-counting:", err)
	}
}

// PreflightRequest projects an issue request onto what the preflight needs, so a
// caller can run the checks for an operation it is about to start without
// restating the fields.
func (r SSLIssueRequest) PreflightRequest() SSLPreflightRequest {
	return SSLPreflightRequest{
		Identifiers: r.Identifiers,
		Challenge:   r.Challenge,
		Op:          r.Op,
		Staging:     r.Staging,
		WebrootPath: r.WebrootPath,
	}
}

// opArgs picks the invocation for the operation.
func (d *sslAcmeDriver) opArgs(req SSLIssueRequest) ([]string, error) {
	switch req.Op {
	case SSLOpIssue:
		return d.issueArgs(req)
	case SSLOpRenew:
		return d.renewArgs(req)
	default:
		return nil, fmt.Errorf("unknown operation %q", req.Op)
	}
}

// sslRedactArgs renders the command for the log. There is nothing secret in argv
// by construction (the Cloudflare token goes through the environment), so this is
// only about readability.
func sslRedactArgs(args []string) string {
	return "acme.sh " + strings.Join(args, " ")
}

// sslExplainFailure turns "acme.sh exited non-zero" into the specific thing to go
// and fix. The CA's own error names nothing actionable, and the three challenges
// fail for three completely different reasons, so one generic message would send
// the operator to the wrong place two times out of three.
func sslExplainFailure(req SSLIssueRequest, out string, err error) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Let's Encrypt did not issue a certificate for %s. ", req.Primary())
	switch req.Challenge {
	case SSLChallengeCloudflareDNS:
		b.WriteString("Validation was over DNS: the API token needs Zone:DNS:Edit on THIS zone (not only on another one), and the zone has to be active in Cloudflare, meaning its nameservers are delegated there.")
	case SSLChallengeStandaloneIP:
		b.WriteString("Let's Encrypt validates an IP certificate by connecting to that address itself on TCP port 80, so it has to be this machine's own public address (not a NAT front, not a CDN) and the port has to be reachable from the internet.")
	case SSLChallengeWebroot:
		// The standalone advice below is actively wrong here: nothing of ours binds
		// port 80, so "stop whatever is holding it" would tell the operator to break
		// the very webserver this challenge depends on.
		fmt.Fprintf(&b, "acme.sh wrote the challenge file into %s/.well-known/acme-challenge/ and Let's Encrypt then fetched it over HTTP. Check that the webserver's vhost for this name really has that directory as its root (nginx `root`, apache `DocumentRoot`), that it does not intercept /.well-known/ with a rewrite or an auth rule, and that TCP port 80 is reachable from the internet.", strings.TrimRight(req.WebrootPath, "/"))
	default:
		b.WriteString("Let's Encrypt validates over HTTP: the name has to resolve to THIS server's public address and TCP port 80 has to be reachable from the internet (not firewalled, not behind a proxy for a different host).")
	}
	b.WriteString(" The certificate in use was left unchanged.")
	if err != nil {
		fmt.Fprintf(&b, " (acme.sh: %v)", err)
	}
	// Only ever one line of acme.sh output in the summary; the whole log is on the
	// step itself.
	if line := sslLastErrorLine(out); line != "" {
		fmt.Fprintf(&b, " Last error: %s", line)
	}
	return b.String()
}

func sslLastErrorLine(out string) string {
	var last string
	for _, line := range strings.Split(out, "\n") {
		l := strings.TrimSpace(line)
		if l == "" {
			continue
		}
		if strings.Contains(l, "error") || strings.Contains(l, "Error") || strings.Contains(l, "invalid") {
			last = l
		}
	}
	if len(last) > 300 {
		last = last[:300]
	}
	return last
}

// RenewIfDue renews the active certificate when it is due, and is the intended
// entry point for a scheduled job.
//
// Deliberately NOT wired to a timer here, and deliberately NOT delegated to
// acme.sh's own cron: acme.sh's `--install` cron would be a SECOND scheduler, and
// two --standalone runs racing for port 80 fail validation, which costs the
// hourly budget. One scheduler, and it is this one.
func (s *SSLService) RenewIfDue() error {
	root := DefaultSSLStoreRoot()
	store, err := OpenSSLStore(root)
	if err != nil {
		return err
	}
	if !store.HasActive() {
		return nil
	}
	info, err := store.ActiveInfo()
	if err != nil {
		return err
	}
	if !info.RenewalDue {
		return nil
	}
	if err := sslCheckMinAge(info, time.Now()); err != nil {
		logger.Warning("ssl: skipping an automatic renewal:", err)
		return nil
	}
	// The challenge here is INERT and is not worth improving. On a renew,
	// renewArgs sends only `--home --config-home --server --renew -d <primary>`
	// and acme.sh replays what it recorded at issue time; the preflight resolves
	// the real one from acme.sh's own conf (sslResolveRenewChallenge). It is set
	// only because the field exists. A renewal that genuinely needs a DIFFERENT
	// challenge is not a renewal: it is an --issue, and it costs an exact-set slot.
	challenge := SSLChallengeStandaloneDomain
	if len(info.IPAddresses) > 0 && len(info.DNSNames) == 0 {
		challenge = SSLChallengeStandaloneIP
	}
	req := SSLOperationRequest{SSLIssueRequest: SSLIssueRequest{
		Identifiers: info.Identifiers,
		Challenge:   challenge,
		Op:          SSLOpRenew,
	}}

	// Check BEFORE starting, so a scheduled renewal that is going to be refused
	// stays silent instead of clobbering the run log.
	//
	// Starting it anyway costs no budget (the run dies in the preflight without
	// contacting the CA and without a ledger entry), but Start resets sslRun, so
	// an operator who ran something by hand would come back to a "recent failures"
	// block from a timer they never triggered, replacing the output they were
	// reading. This belongs here rather than in the job: the job cannot know which
	// refusals are routine, and every caller of RenewIfDue wants the same silence.
	pre, err := s.Preflight(req.PreflightRequest())
	if err != nil {
		return err
	}
	if pre.Blocked {
		logger.Info("ssl: automatic renewal not attempted:", pre.Reason)
		return nil
	}
	return s.Start(req)
}

// SSLStoreExists reports whether anything has been issued into the managed store,
// so the UI can show an empty state rather than an error on a fresh install.
func SSLStoreExists() bool {
	_, err := os.Stat(filepath.Join(DefaultSSLStoreRoot(), sslVersionsDir))
	return err == nil
}
