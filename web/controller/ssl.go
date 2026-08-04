package controller

import (
	"errors"
	"strings"

	"github.com/mhsanaei/3x-ui/v2/web/service"

	"github.com/gin-gonic/gin"
)

// The SSL certificate manager's HTTP surface. Routes are registered in
// setting.go's initRouter so the gating sits next to saveService's, which is the
// only other escalation-class route on that group.
//
// THE SPLIT, AND WHY IT IS WHERE IT IS. Reading rides the group's existing
// PermPanelSettings gate: a status page reveals which names this host holds a
// certificate for, which the same admin already reads off the inbound forms.
// Every MUTATING route carries its own requireSuperAdmin(), for the reason stated
// at core.go:44-49: driving acme.sh as a subprocess and then writing the files the
// webserver loads as its own TLS identity is equivalent to running code as root
// here, and no permission bit stands in for that.
//
// THE CLOUDFLARE TOKEN NEVER LEAVES THIS FILE. It is read straight out of the
// form into the service request and is never logged, never stored in a setting and
// never present in any response: SSLStatus has no field for it and
// SSLIssueRequest.CloudflareToken is `json:"-"` (sslacme.go:206), so even echoing
// the whole request back cannot leak it. The service in turn hands it to acme.sh
// through the ENVIRONMENT rather than argv, because /proc/<pid>/cmdline is
// world-readable; see sslmanager.go:347.

// sslStartForm is one operator action.
//
// Every panel POST is x-www-form-urlencoded: the axios interceptor at
// assets/js/axios-init.js:9-11 runs Qs.stringify over every body, so these need
// `form:` tags and JSON tags alone would bind nothing.
type sslStartForm struct {
	// Identifiers arrive REPEATED (identifiers=a&identifiers=b), because that same
	// interceptor stringifies arrays with arrayFormat:'repeat'. Gin binds a []string
	// from repeated keys directly, so nothing here splits on a comma and a name can
	// never be torn in half by one.
	//
	// The ORDER is load-bearing and deliberately preserved rather than sorted: the
	// first identifier is the name acme.sh files the certificate under and addresses
	// every later --renew and --install-cert by (sslacme.go:190-195). The ledger's
	// own sorted key is derived separately.
	//
	// An EMPTY array posts NOTHING at all under arrayFormat:'repeat', so an absent
	// field arrives as an empty list rather than as an error. That is left to the
	// service, whose preflight already refuses it with a message that says what to
	// type; rejecting it here would only duplicate that text in a second place.
	Identifiers []string `form:"identifiers" json:"identifiers"`

	Challenge string `form:"challenge" json:"challenge"`
	Op        string `form:"op" json:"op"`
	Staging   bool   `form:"staging" json:"staging"`
	Email     string `form:"email" json:"email"`
	ListenV6  bool   `form:"listenV6" json:"listenV6"`

	// WebrootPath is read only for the webroot challenge, where the operator's own
	// webserver answers the check and acme.sh only drops a file under this
	// directory. The service validates it; an absolute path is not enough on its
	// own and nothing here pretends otherwise.
	WebrootPath string `form:"webrootPath" json:"webrootPath"`

	// ApplyDisruptive restarts the consumers that can only pick up a new certificate
	// by restarting (ocserv, accel-ppp, one-time-loading Xray inbounds), dropping
	// every connected user. Opt-in, and it stays opt-in: absent means false, so an
	// old client or a hand-made request can never disconnect anyone by omission.
	ApplyDisruptive bool `form:"applyDisruptive" json:"applyDisruptive"`
}

// sslOperationRequest builds the service request from the form.
func sslOperationRequest(c *gin.Context) (service.SSLOperationRequest, error) {
	var f sslStartForm
	if err := c.ShouldBind(&f); err != nil {
		return service.SSLOperationRequest{}, err
	}
	req := service.SSLOperationRequest{
		SSLIssueRequest: service.SSLIssueRequest{
			Identifiers: f.Identifiers,
			Challenge:   strings.TrimSpace(f.Challenge),
			Op:          strings.TrimSpace(f.Op),
			Staging:     f.Staging,
			Email:       strings.TrimSpace(f.Email),
			ListenV6:    f.ListenV6,
			WebrootPath: strings.TrimSpace(f.WebrootPath),
			// Read out of the form directly and never bound into the struct above,
			// so it cannot ride along into a log line, a response body or a debug
			// dump of the form. See the file comment.
			CloudflareToken: c.PostForm("cloudflareToken"),
		},
		FanOut: service.SSLFanOutOptions{ApplyDisruptive: f.ApplyDisruptive},
	}
	return req, nil
}

// sslStatus is the whole picture for the settings page in one call: the active
// certificate, whether the panel is actually serving it, the consumers, the
// budget, the history and the rollback versions.
func (a *SettingController) sslStatus(c *gin.Context) {
	st, err := a.sslService.Status()
	if err != nil {
		jsonMsg(c, I18nWeb(c, "pages.settings.ssl.toasts.status"), err)
		return
	}
	jsonObj(c, st, nil)
}

// sslRunStatus is the poll target for the live console.
//
// Two endpoints (start, then poll) rather than one streaming response, copying
// core.go:95-123 for the reason given at core.html:980-984: the run is server-side,
// so progress survives the operator closing the panel and works through gzip and
// reverse proxies that would buffer an event stream.
//
// It returns SSLRunState unchanged, which is deliberately the same shape as the
// provisioning run, so the existing setup-console renderer applies to it with no
// translation layer in between.
func (a *SettingController) sslRunStatus(c *gin.Context) {
	jsonObj(c, a.sslService.RunState(), nil)
}

// sslPreflight runs every local check and reports the verdict WITHOUT contacting a
// CA, so the operator sees what would fail before spending anything.
//
// A POST because it carries the identifier list, but a READ: it changes nothing and
// costs no budget, so it stays on the group's PermPanelSettings gate rather than
// taking the super-admin one. Being free is the point of it, and a gate the panel
// admin cannot pass would push them straight to the button that is not free.
func (a *SettingController) sslPreflight(c *gin.Context) {
	req, err := sslOperationRequest(c)
	if err != nil {
		jsonMsg(c, I18nWeb(c, "pages.settings.ssl.toasts.preflight"), err)
		return
	}
	res, err := a.sslService.Preflight(req.PreflightRequest())
	if err != nil {
		jsonMsg(c, I18nWeb(c, "pages.settings.ssl.toasts.preflight"), err)
		return
	}
	jsonObj(c, res, nil)
}

// sslConsumers lists everything on this host configured to serve the managed
// certificate, with the per-consumer cost of applying a new one.
func (a *SettingController) sslConsumers(c *gin.Context) {
	list, err := a.sslService.Consumers()
	if err != nil {
		jsonMsg(c, I18nWeb(c, "pages.settings.ssl.toasts.consumers"), err)
		return
	}
	jsonObj(c, list, nil)
}

// sslStart launches an issue / renew / re-apply in the background and returns the
// initial run state; the client then polls sslRunStatus.
//
// A refusal (another run in flight, a drained budget, a cooldown) arrives as a
// failed Msg carrying the service's own sentence, which already names the limit and
// the wall-clock time it frees. Nothing is rewritten here: the message the operator
// needs is the one the ledger computed.
func (a *SettingController) sslStart(c *gin.Context) {
	req, err := sslOperationRequest(c)
	if err != nil {
		jsonMsg(c, I18nWeb(c, "pages.settings.ssl.toasts.start"), err)
		return
	}
	if err := a.sslService.Start(req); err != nil {
		jsonMsg(c, I18nWeb(c, "pages.settings.ssl.toasts.start"), err)
		return
	}
	jsonObj(c, a.sslService.RunState(), nil)
}

// sslUseManaged points the panel's own listener (and the subscription listener) at
// the store's stable paths.
//
// The one-click fix for the single most confusing state this feature has: a
// certificate that renews perfectly while the panel keeps serving something else,
// because webCertFile still names a file the renewal never touches.
func (a *SettingController) sslUseManaged(c *gin.Context) {
	err := a.sslService.UseManagedCertificate()
	jsonMsg(c, I18nWeb(c, "pages.settings.ssl.toasts.useManaged"), err)
}

// sslRollback re-points the active link at a stored version.
//
// The version is whatever the client sends, so it is a path from an HTTP request:
// the service refuses anything outside the store (sslmanager.go:188-193) and
// revalidates the pair before activating it, so a rollback cannot be the thing that
// breaks TLS either. Refusing an empty value here only saves a round trip.
func (a *SettingController) sslRollback(c *gin.Context) {
	version := strings.TrimSpace(c.PostForm("version"))
	if version == "" {
		jsonMsg(c, I18nWeb(c, "pages.settings.ssl.toasts.rollback"), errors.New("no version was given"))
		return
	}
	err := a.sslService.Rollback(version)
	jsonMsg(c, I18nWeb(c, "pages.settings.ssl.toasts.rollback"), err)
}
