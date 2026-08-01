package service

import (
	"testing"

	"github.com/mhsanaei/3x-ui/v2/database"
	"github.com/mhsanaei/3x-ui/v2/database/model"
)

// One account, several inbounds, one quota. These pin the feature working, which
// is also what makes the IDOR cases in web/controller meaningful: if inboundIds
// were ignored outright those would pass vacuously.

// The headline: an account added on one inbound and given a membership on a
// second appears on BOTH, with the credential each protocol keys on.
func TestApplyMembershipsSpreadsAccountAcrossProtocols(t *testing.T) {
	svc := newAccountsDB(t)
	vless := seedInboundWithClients(t, model.VLESS, 46101, []map[string]any{
		{"id": "3fa85f64-5717-4562-b3fc-2c963f66afa6", "email": "bob@example.com", "enable": true, "totalGB": float64(100)},
	})
	l2tp := seedInboundWithClients(t, model.L2TP, 46102, []map[string]any{})
	svc.MigrationAccounts()

	// Give the account the l2tp credential fields it will need there.
	account, err := svc.GetAccountByEmail("bob@example.com")
	if err != nil || account == nil {
		t.Fatalf("GetAccountByEmail: %v", err)
	}
	account.VpnUsername = "bob-login"
	account.Password = "bob-pw"
	if err := database.GetDB().Save(account).Error; err != nil {
		t.Fatalf("save account: %v", err)
	}

	touched, err := svc.ApplyMemberships("bob@example.com", []int{vless.Id, l2tp.Id}, nil, true)
	if err != nil {
		t.Fatalf("ApplyMemberships: %v", err)
	}
	if len(touched) == 0 {
		t.Fatal("no inbound was reported as changed")
	}

	l2tpClients := readClients(t, l2tp.Id)
	if len(l2tpClients) != 1 {
		t.Fatalf("l2tp has %d clients, want 1", len(l2tpClients))
	}
	if got, _ := l2tpClients[0]["email"].(string); got != "bob@example.com" {
		t.Errorf("l2tp client email = %q", got)
	}
	if got, _ := l2tpClients[0]["id"].(string); got != "bob-login" {
		t.Errorf("l2tp id = %q, want the vpn username: without it RADIUS has nothing to authenticate", got)
	}
	if got, _ := l2tpClients[0]["password"].(string); got != "bob-pw" {
		t.Errorf("l2tp password = %q", got)
	}
	// The quota is the ACCOUNT's, carried onto the new membership.
	if got, _ := l2tpClients[0]["totalGB"].(float64); got != 100 {
		t.Errorf("l2tp totalGB = %v, want the account's 100", got)
	}
	// A pool protocol must get an explicit slot, or its tunnel address is decided
	// by list position.
	if _, ok := l2tpClients[0]["slot"]; !ok {
		t.Error("the new l2tp membership has no explicit slot")
	}

	// And the vless side keeps its own credential, untouched.
	vlessClients := readClients(t, vless.Id)
	if got, _ := vlessClients[0]["id"].(string); got != "3fa85f64-5717-4562-b3fc-2c963f66afa6" {
		t.Errorf("vless uuid changed to %q", got)
	}

	if got := len(accountsInDB(t)); got != 1 {
		t.Errorf("accounts = %d, want 1: this is ONE account on two inbounds", got)
	}
	if got := len(membershipsInDB(t)); got != 2 {
		t.Errorf("memberships = %d, want 2", got)
	}
}

// Unticking an inbound removes the account from it. A membership left behind is
// a working account nobody is billed for.
func TestApplyMembershipsRemovesDroppedInbound(t *testing.T) {
	svc := newAccountsDB(t)
	vless := seedInboundWithClients(t, model.VLESS, 46201, []map[string]any{
		{"id": "3fa85f64-5717-4562-b3fc-2c963f66afa6", "email": "bob@example.com", "enable": true},
	})
	trojan := seedInboundWithClients(t, model.Trojan, 46202, []map[string]any{
		{"password": "pw", "email": "bob@example.com", "enable": true},
	})
	svc.MigrationAccounts()
	if got := len(membershipsInDB(t)); got != 2 {
		t.Fatalf("setup: memberships = %d, want 2", got)
	}

	if _, err := svc.ApplyMemberships("bob@example.com", []int{vless.Id}, []int{trojan.Id}, true); err != nil {
		t.Fatalf("ApplyMemberships: %v", err)
	}

	if got := len(readClients(t, trojan.Id)); got != 0 {
		t.Errorf("the dropped inbound still carries %d clients: a live account nobody is billed for", got)
	}
	if got := len(readClients(t, vless.Id)); got != 1 {
		t.Errorf("the kept inbound has %d clients, want 1", got)
	}
	if got := len(membershipsInDB(t)); got != 1 {
		t.Errorf("memberships = %d, want 1", got)
	}
}

// Removing is authorized by owning the inbound being removed FROM, which is a
// different set from the one being added to. An admin who edits a shared account
// without ticking an inbound they cannot even see must leave it alone: silently
// unprovisioning it would be an IDOR in the removal direction.
func TestApplyMembershipsKeepsMembershipsTheCallerCannotRemove(t *testing.T) {
	svc := newAccountsDB(t)
	mine := seedInboundWithClients(t, model.VLESS, 46601, []map[string]any{
		{"id": "3fa85f64-5717-4562-b3fc-2c963f66afa6", "email": "bob@example.com", "enable": true},
	})
	theirs := seedInboundWithClients(t, model.Trojan, 46602, []map[string]any{
		{"password": "pw", "email": "bob@example.com", "enable": true},
	})
	svc.MigrationAccounts()

	// The caller asks for only their own inbound, and is allowed to remove nothing.
	if _, err := svc.ApplyMemberships("bob@example.com", []int{mine.Id}, nil, true); err != nil {
		t.Fatalf("ApplyMemberships: %v", err)
	}

	if got := len(readClients(t, theirs.Id)); got != 1 {
		t.Errorf("the account was removed from an inbound the caller may not touch (%d clients left)", got)
	}
	if got := len(membershipsInDB(t)); got != 2 {
		t.Errorf("memberships = %d, want 2: the unowned one must survive", got)
	}
}

// mergeKeepSet is the rule itself, tested directly.
func TestMergeKeepSet(t *testing.T) {
	cases := []struct {
		name                       string
		wanted, current, removable []int
		want                       []int
	}{
		{"adds what was asked for", []int{1, 2}, []int{1}, []int{}, []int{1, 2}},
		{"drops only what may be dropped", []int{1}, []int{1, 2}, []int{2}, []int{1}},
		{"keeps what may not be dropped", []int{1}, []int{1, 2}, []int{}, []int{1, 2}},
		{"mixed", []int{1, 3}, []int{1, 2, 4}, []int{2}, []int{1, 3, 4}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mergeKeepSet(tc.wanted, tc.current, tc.removable)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// A single-inbound request must behave exactly as before. This is what protects
// every existing caller (the Telegram bot, bulk ops, external scripts) on upgrade.
func TestApplyMembershipsSingleInboundIsInert(t *testing.T) {
	svc := newAccountsDB(t)
	inbound := seedInboundWithClients(t, model.VLESS, 46301, []map[string]any{
		{"id": "3fa85f64-5717-4562-b3fc-2c963f66afa6", "email": "bob@example.com", "enable": true},
	})
	svc.MigrationAccounts()
	before := snapshotUntouchedTables(t)

	if _, err := svc.ApplyMemberships("bob@example.com", []int{inbound.Id}, nil, false); err != nil {
		t.Fatalf("ApplyMemberships: %v", err)
	}

	assertAdditiveOnly(t, before)
}

// Two l2tp inbounds share one daemon that sends a bare "l2tp" NAS-Identifier, so
// RADIUS resolves the account to whichever inbound has the lower id. The account
// would be created, listed on both, log in fine, and always be served by one of
// them, taking ITS ranges and user limit. Refused rather than silently accepted.
func TestValidateMembershipSetRefusesAmbiguousSameProtocol(t *testing.T) {
	svc := newAccountsDB(t)
	for _, protocol := range []model.Protocol{model.L2TP, model.PPTP, model.IKEV2} {
		first := &model.Inbound{Protocol: protocol, Remark: "first", Id: 1}
		second := &model.Inbound{Protocol: protocol, Remark: "second", Id: 2}
		if err := svc.ValidateMembershipSet([]*model.Inbound{first, second}); err == nil {
			t.Errorf("%s: two memberships accepted, but the shared daemon cannot tell them apart", protocol)
		}
	}
}

// openvpn, openconnect and sstp already send "<proto>-<inboundId>", so RADIUS
// resolves them exactly and two memberships are safe.
func TestValidateMembershipSetAllowsPerInboundNasProtocols(t *testing.T) {
	svc := newAccountsDB(t)
	for _, protocol := range []model.Protocol{model.OPENVPN, model.OPENCONNECT, model.SSTP} {
		first := &model.Inbound{Protocol: protocol, Remark: "first", Id: 1}
		second := &model.Inbound{Protocol: protocol, Remark: "second", Id: 2}
		if err := svc.ValidateMembershipSet([]*model.Inbound{first, second}); err != nil {
			t.Errorf("%s: refused, but it sends a per-inbound NAS-Identifier and resolves exactly: %v", protocol, err)
		}
	}
}

// Different protocols are the whole point and must always be allowed.
func TestValidateMembershipSetAllowsDifferentProtocols(t *testing.T) {
	svc := newAccountsDB(t)
	err := svc.ValidateMembershipSet([]*model.Inbound{
		{Protocol: model.L2TP, Id: 1}, {Protocol: model.PPTP, Id: 2},
		{Protocol: model.VLESS, Id: 3}, {Protocol: model.WGC, Id: 4},
	})
	if err != nil {
		t.Errorf("one account across four different protocols was refused: %v", err)
	}
}

// An account whose last membership goes away must not linger: the email would
// read as taken and refuse a later re-create of the same customer.
func TestSyncInboundAccountsPrunesOrphans(t *testing.T) {
	svc := newAccountsDB(t)
	inbound := seedInboundWithClients(t, model.VLESS, 46401, []map[string]any{
		{"id": "3fa85f64-5717-4562-b3fc-2c963f66afa6", "email": "bob@example.com", "enable": true},
	})
	svc.MigrationAccounts()
	if len(accountsInDB(t)) != 1 {
		t.Fatal("setup: expected one account")
	}

	// Emulate a plain client delete through the legacy path.
	if err := database.GetDB().Model(&model.Inbound{}).Where("id = ?", inbound.Id).
		Update("settings", `{"clients":[]}`).Error; err != nil {
		t.Fatalf("clear clients: %v", err)
	}
	if err := svc.SyncInboundAccounts(database.GetDB(), inbound.Id); err != nil {
		t.Fatalf("SyncInboundAccounts: %v", err)
	}

	if got := len(membershipsInDB(t)); got != 0 {
		t.Errorf("memberships = %d, want 0", got)
	}
	if got := len(accountsInDB(t)); got != 0 {
		t.Errorf("accounts = %d, want 0: an account with no membership is addressable by nothing", got)
	}
}

// Deleting the inbound itself drops its memberships but must not delete accounts
// that are still served elsewhere.
func TestSyncInboundAccountsKeepsAccountAliveOnOtherInbounds(t *testing.T) {
	svc := newAccountsDB(t)
	keep := seedInboundWithClients(t, model.VLESS, 46501, []map[string]any{
		{"id": "3fa85f64-5717-4562-b3fc-2c963f66afa6", "email": "bob@example.com", "enable": true},
	})
	gone := seedInboundWithClients(t, model.Trojan, 46502, []map[string]any{
		{"password": "pw", "email": "bob@example.com", "enable": true},
	})
	svc.MigrationAccounts()

	if err := database.GetDB().Where("id = ?", gone.Id).Delete(&model.Inbound{}).Error; err != nil {
		t.Fatalf("delete inbound: %v", err)
	}
	if err := svc.SyncInboundAccounts(database.GetDB(), gone.Id); err != nil {
		t.Fatalf("SyncInboundAccounts: %v", err)
	}

	if got := len(accountsInDB(t)); got != 1 {
		t.Fatalf("accounts = %d, want 1: the account is still served on the other inbound", got)
	}
	ids, err := svc.InboundIdsForEmail("bob@example.com")
	if err != nil {
		t.Fatalf("InboundIdsForEmail: %v", err)
	}
	if len(ids) != 1 || ids[0] != keep.Id {
		t.Errorf("memberships = %v, want just [%d]", ids, keep.Id)
	}
}
