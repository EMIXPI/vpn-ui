package service

import (
	"testing"

	"github.com/mhsanaei/3x-ui/v2/database"
	"github.com/mhsanaei/3x-ui/v2/database/model"
	"github.com/mhsanaei/3x-ui/v2/xray"
)

// Per-inbound traffic attribution.
//
// The bug these pin: an account on two inbounds has ONE client_traffics row (email
// is unique panel-wide), so every byte it moved was rendered under whichever inbound
// happened to create that row, and the others showed nothing at all. The account
// total was never wrong; the split was invented.
//
// The invariant throughout is that the breakdown SUMS TO the account total, minus
// only what named no source inbound. If a change here makes the two disagree, the
// panel is billing one number and displaying another.

// membershipUsageOf reads one membership's stored share.
func membershipUsageOf(t *testing.T, email string, inboundId int) model.AccountInbound {
	t.Helper()
	var row model.AccountInbound
	err := database.GetDB().Table("account_inbounds").
		Select("account_inbounds.*").
		Joins("JOIN accounts ON accounts.id = account_inbounds.account_id").
		Where("account_inbounds.inbound_id = ? AND LOWER(TRIM(accounts.email)) = ?", inboundId, accountKey(email)).
		Scan(&row).Error
	if err != nil {
		t.Fatalf("read membership usage (%s on inbound %d): %v", email, inboundId, err)
	}
	return row
}

func accountTrafficOf(t *testing.T, email string) xray.ClientTraffic {
	t.Helper()
	var row xray.ClientTraffic
	if err := database.GetDB().Model(xray.ClientTraffic{}).
		Where("email = ?", email).First(&row).Error; err != nil {
		t.Fatalf("read client_traffics for %s: %v", email, err)
	}
	return row
}

// seedTwoInboundAccount puts one account on an openvpn and an ikev2 inbound, which
// is the exact shape the bug was reported on, and returns the two.
//
// openvpn deliberately takes the LOWER id: memberships apply in ascending inbound
// order, so it is the one that wins the single client_traffics row, and it is what
// used to be credited with the ikev2 traffic as well.
func seedTwoInboundAccount(t *testing.T, svc *AccountService, email string) (*model.Inbound, *model.Inbound) {
	t.Helper()
	openvpn := seedInboundWithClients(t, model.OPENVPN, 47101, []map[string]any{
		{"id": "acct-login", "email": email, "enable": true, "totalGB": float64(0)},
	})
	ikev2 := seedInboundWithClients(t, model.IKEV2, 47102, []map[string]any{})
	svc.MigrationAccounts()

	account, err := svc.GetAccountByEmail(email)
	if err != nil || account == nil {
		t.Fatalf("GetAccountByEmail(%s): %v (err)", email, err)
	}
	account.VpnUsername = "acct-login"
	account.Password = "acct-pw"
	if err := database.GetDB().Save(account).Error; err != nil {
		t.Fatalf("save account: %v", err)
	}
	if _, err := svc.ApplyMemberships(email, []int{openvpn.Id, ikev2.Id}, nil, true); err != nil {
		t.Fatalf("ApplyMemberships: %v", err)
	}
	seedAccountTraffic(t, email, openvpn.Id)
	return openvpn, ikev2
}

// seedAccountTraffic creates the account's one counter row, homed on the inbound
// that would have created it. Nothing in the accounts layer writes client_traffics,
// so a test that never adds a client through the service has to do it here.
func seedAccountTraffic(t *testing.T, email string, homeInboundId int) {
	t.Helper()
	if err := database.GetDB().Create(&xray.ClientTraffic{
		InboundId: homeInboundId, Email: email, Enable: true,
	}).Error; err != nil {
		t.Fatalf("seed client_traffics for %s: %v", email, err)
	}
}

// The headline. Two collected records, each naming its own inbound, and each
// membership accumulates only its own bytes rather than both landing on one.
func TestAddClientTrafficSplitsBySourceInbound(t *testing.T) {
	svc := newAccountsDB(t)
	const email = "split@example.com"
	openvpn, ikev2 := seedTwoInboundAccount(t, svc, email)

	inbounds := &InboundService{}
	err, _, _, _, _ := inbounds.AddTraffic(nil, []*xray.ClientTraffic{
		{InboundId: openvpn.Id, Email: email, Up: 3 * mb, Down: 7 * mb},
		{InboundId: ikev2.Id, Email: email, Up: 1 * mb, Down: 9 * mb},
	})
	if err != nil {
		t.Fatalf("AddTraffic: %v", err)
	}

	ovpn := membershipUsageOf(t, email, openvpn.Id)
	if ovpn.Up != 3*mb || ovpn.Down != 7*mb {
		t.Errorf("openvpn membership = (up %d, down %d); want (%d, %d)", ovpn.Up, ovpn.Down, 3*mb, 7*mb)
	}
	ike := membershipUsageOf(t, email, ikev2.Id)
	if ike.Up != 1*mb || ike.Down != 9*mb {
		t.Errorf("ikev2 membership = (up %d, down %d); want (%d, %d). "+
			"Zero here is the reported bug: the ikev2 usage was credited to openvpn.", ike.Up, ike.Down, 1*mb, 9*mb)
	}

	// And the account row is untouched by the split: it still carries the whole of
	// it, because that is the row every enforcement path reads.
	total := accountTrafficOf(t, email)
	if total.Up != 4*mb || total.Down != 16*mb {
		t.Errorf("account total = (up %d, down %d); want (%d, %d)", total.Up, total.Down, 4*mb, 16*mb)
	}

	// The invariant.
	if got, want := ovpn.Up+ovpn.Down+ike.Up+ike.Down, total.Up+total.Down; got != want {
		t.Errorf("the breakdown sums to %d but the account total is %d", got, want)
	}
	if got, want := ovpn.AllTime+ike.AllTime, total.AllTime; got != want {
		t.Errorf("the all-time breakdown sums to %d but the account's all_time is %d", got, want)
	}
}

// A record that names no inbound still reaches the account total. It is left OUT of
// the breakdown deliberately: the Xray-native protocols report per account with no
// inbound in the stat name, and filing those bytes under a guess is the whole bug.
func TestAddClientTrafficUnattributedReachesTotalOnly(t *testing.T) {
	svc := newAccountsDB(t)
	const email = "unattributed@example.com"
	openvpn, ikev2 := seedTwoInboundAccount(t, svc, email)

	inbounds := &InboundService{}
	err, _, _, _, _ := inbounds.AddTraffic(nil, []*xray.ClientTraffic{
		{InboundId: openvpn.Id, Email: email, Up: 0, Down: 4 * mb},
		{InboundId: 0, Email: email, Up: 0, Down: 6 * mb},
	})
	if err != nil {
		t.Fatalf("AddTraffic: %v", err)
	}

	total := accountTrafficOf(t, email)
	if got, want := total.Up+total.Down, int64(10*mb); got != want {
		t.Errorf("account total = %d; want %d. Bytes must never be dropped just because "+
			"their source inbound is unknown.", got, want)
	}

	ovpn := membershipUsageOf(t, email, openvpn.Id)
	if ovpn.Down != 4*mb {
		t.Errorf("openvpn membership down = %d; want %d", ovpn.Down, 4*mb)
	}
	ike := membershipUsageOf(t, email, ikev2.Id)
	if ike.Up != 0 || ike.Down != 0 {
		t.Errorf("ikev2 membership = (up %d, down %d); want zero. An unattributed record "+
			"must not be spread over the memberships.", ike.Up, ike.Down)
	}

	// The gap is the point: the breakdown is short by exactly the unattributed part,
	// which is what attachClientStats renders as the Xray-native remainder.
	if got, want := ovpn.Up+ovpn.Down+ike.Up+ike.Down, int64(4*mb); got != want {
		t.Errorf("breakdown = %d; want %d (the total minus the unattributed 6 MiB)", got, want)
	}
}

// Resetting an account's traffic has to clear the breakdown too, or the split keeps
// claiming bytes the account no longer has and totals more than it.
func TestResetClientTrafficClearsTheBreakdown(t *testing.T) {
	svc := newAccountsDB(t)
	const email = "reset@example.com"
	openvpn, ikev2 := seedTwoInboundAccount(t, svc, email)

	inbounds := &InboundService{}
	err, _, _, _, _ := inbounds.AddTraffic(nil, []*xray.ClientTraffic{
		{InboundId: openvpn.Id, Email: email, Up: 5 * mb, Down: 5 * mb},
		{InboundId: ikev2.Id, Email: email, Up: 5 * mb, Down: 5 * mb},
	})
	if err != nil {
		t.Fatalf("AddTraffic: %v", err)
	}
	if err := inbounds.ResetClientTrafficByEmail(email); err != nil {
		t.Fatalf("ResetClientTrafficByEmail: %v", err)
	}

	for _, id := range []int{openvpn.Id, ikev2.Id} {
		row := membershipUsageOf(t, email, id)
		if row.Up != 0 || row.Down != 0 {
			t.Errorf("inbound %d still holds (up %d, down %d) after a reset", id, row.Up, row.Down)
		}
		// all_time survives a reset on the account row, so it must survive here too.
		if row.AllTime != 10*mb {
			t.Errorf("inbound %d all_time = %d; want %d. A reset clears what counts "+
				"against the quota, never the lifetime record.", id, row.AllTime, 10*mb)
		}
	}
}

// The backfill is honest rather than flattering: an account's whole existing usage
// goes onto the membership its client_traffics row already named, freezing today's
// wrong attribution as the historical bucket. Dividing it evenly across memberships
// would be inventing data that was never measured.
func TestMigrationMembershipUsageSeedsTheHomeInbound(t *testing.T) {
	svc := newAccountsDB(t)
	const email = "backfill@example.com"
	openvpn, ikev2 := seedTwoInboundAccount(t, svc, email)

	// Usage as it exists before the split ever ran: all on the account row.
	if err := database.GetDB().Model(xray.ClientTraffic{}).Where("email = ?", email).
		Updates(map[string]any{"inbound_id": openvpn.Id, "up": 30 * mb, "down": 70 * mb, "all_time": 100 * mb}).
		Error; err != nil {
		t.Fatalf("seed usage: %v", err)
	}

	svc.MigrationMembershipUsage()

	home := membershipUsageOf(t, email, openvpn.Id)
	if home.Up != 30*mb || home.Down != 70*mb || home.AllTime != 100*mb {
		t.Errorf("home membership = (up %d, down %d, allTime %d); want (%d, %d, %d)",
			home.Up, home.Down, home.AllTime, 30*mb, 70*mb, 100*mb)
	}
	other := membershipUsageOf(t, email, ikev2.Id)
	if other.Up != 0 || other.Down != 0 || other.AllTime != 0 {
		t.Errorf("the other membership = (up %d, down %d, allTime %d); want zero. "+
			"There is no historical split to recover, so none may be invented.",
			other.Up, other.Down, other.AllTime)
	}

	total := accountTrafficOf(t, email)
	if got, want := home.Up+home.Down+other.Up+other.Down, total.Up+total.Down; got != want {
		t.Errorf("after the backfill the breakdown sums to %d but the account total is %d", got, want)
	}

	// Idempotent: a second start must not double the seeded figures.
	svc.MigrationMembershipUsage()
	if again := membershipUsageOf(t, email, openvpn.Id); again.Up != 30*mb || again.Down != 70*mb {
		t.Errorf("a second pass changed the seeded usage to (up %d, down %d)", again.Up, again.Down)
	}
}

// An account whose home inbound has been deleted is common: nothing prunes
// client_traffics.inbound_id when its inbound goes. Its history must land on a
// membership that exists rather than being dropped on the floor.
func TestMigrationMembershipUsageFallsBackToTheLowestLiveMembership(t *testing.T) {
	svc := newAccountsDB(t)
	const email = "orphan@example.com"
	openvpn, ikev2 := seedTwoInboundAccount(t, svc, email)

	if err := database.GetDB().Model(xray.ClientTraffic{}).Where("email = ?", email).
		Updates(map[string]any{"inbound_id": 9999, "up": 20 * mb, "down": 0, "all_time": 20 * mb}).
		Error; err != nil {
		t.Fatalf("seed usage: %v", err)
	}

	svc.MigrationMembershipUsage()

	low := membershipUsageOf(t, email, openvpn.Id)
	if low.Up != 20*mb {
		t.Errorf("lowest live membership up = %d; want %d", low.Up, 20*mb)
	}
	if high := membershipUsageOf(t, email, ikev2.Id); high.Up != 0 {
		t.Errorf("the higher membership took %d; want 0", high.Up)
	}
}

// The display half. An account on two inbounds must appear under BOTH, each showing
// its own share, and the account totals must be left alone so the quota bars keep
// measuring against the right number.
func TestAttachClientStatsShowsTheAccountOnEveryInbound(t *testing.T) {
	svc := newAccountsDB(t)
	const email = "display@example.com"
	openvpn, ikev2 := seedTwoInboundAccount(t, svc, email)

	inbounds := &InboundService{}
	err, _, _, _, _ := inbounds.AddTraffic(nil, []*xray.ClientTraffic{
		{InboundId: openvpn.Id, Email: email, Up: 0, Down: 2 * mb},
		{InboundId: ikev2.Id, Email: email, Up: 0, Down: 8 * mb},
	})
	if err != nil {
		t.Fatalf("AddTraffic: %v", err)
	}

	loaded, err := inbounds.GetAllInbounds()
	if err != nil {
		t.Fatalf("GetAllInbounds: %v", err)
	}
	byId := map[int]*model.Inbound{}
	for _, in := range loaded {
		byId[in.Id] = in
	}

	for _, tc := range []struct {
		id       int
		name     string
		wantDown int64
	}{
		{openvpn.Id, "openvpn", 2 * mb},
		{ikev2.Id, "ikev2", 8 * mb},
	} {
		in := byId[tc.id]
		if in == nil {
			t.Fatalf("%s inbound missing from the list", tc.name)
		}
		var row *xray.ClientTraffic
		for i := range in.ClientStats {
			if accountKey(in.ClientStats[i].Email) == accountKey(email) {
				row = &in.ClientStats[i]
			}
		}
		if row == nil {
			t.Fatalf("%s inbound does not list the account at all. Before this change an "+
				"account was missing from every inbound but its home one.", tc.name)
		}
		if row.InboundDown != tc.wantDown {
			t.Errorf("%s: inboundDown = %d; want %d", tc.name, row.InboundDown, tc.wantDown)
		}
		// The account totals ride along unchanged on every row: the quota is
		// account-wide, so a progress bar fed one inbound's slice reads empty for a
		// customer who is actually out of traffic.
		if row.Down != 10*mb {
			t.Errorf("%s: the account total down = %d; want %d", tc.name, row.Down, 10*mb)
		}
		if row.InboundId != tc.id {
			t.Errorf("%s: row.inboundId = %d; want the inbound it is rendered under (%d), "+
				"which is what the page sends back as the :id of /resetClientTraffic",
				tc.name, row.InboundId, tc.id)
		}
		if row.Shared {
			t.Errorf("%s counts its own traffic, so its row must not be marked shared", tc.name)
		}
	}
}

// Xray-native inbounds cannot be attributed at all: the core's stat is named
// "user>>><email>>>>traffic" with no inbound in it. Those rows carry the
// unattributed REMAINDER and say so, rather than a zero that reads as "never used".
func TestAttachClientStatsMarksXrayNativeAsShared(t *testing.T) {
	svc := newAccountsDB(t)
	const email = "mixed@example.com"

	openvpn := seedInboundWithClients(t, model.OPENVPN, 47201, []map[string]any{
		{"id": "mixed-login", "email": email, "enable": true, "totalGB": float64(0)},
	})
	vless := seedInboundWithClients(t, model.VLESS, 47202, []map[string]any{})
	svc.MigrationAccounts()
	account, err := svc.GetAccountByEmail(email)
	if err != nil || account == nil {
		t.Fatalf("GetAccountByEmail: %v", err)
	}
	account.VpnUsername = "mixed-login"
	account.Password = "mixed-pw"
	account.UUID = "3fa85f64-5717-4562-b3fc-2c963f66afa6"
	if err := database.GetDB().Save(account).Error; err != nil {
		t.Fatalf("save account: %v", err)
	}
	if _, err := svc.ApplyMemberships(email, []int{openvpn.Id, vless.Id}, nil, true); err != nil {
		t.Fatalf("ApplyMemberships: %v", err)
	}
	seedAccountTraffic(t, email, openvpn.Id)

	inbounds := &InboundService{}
	// 4 MiB the openvpn tunnel counted, 6 MiB Xray reported with no inbound on it.
	err, _, _, _, _ = inbounds.AddTraffic(nil, []*xray.ClientTraffic{
		{InboundId: openvpn.Id, Email: email, Up: 0, Down: 4 * mb},
		{InboundId: 0, Email: email, Up: 0, Down: 6 * mb},
	})
	if err != nil {
		t.Fatalf("AddTraffic: %v", err)
	}

	loaded, err := inbounds.GetAllInbounds()
	if err != nil {
		t.Fatalf("GetAllInbounds: %v", err)
	}
	find := func(inboundId int) *xray.ClientTraffic {
		for _, in := range loaded {
			if in.Id != inboundId {
				continue
			}
			for i := range in.ClientStats {
				if accountKey(in.ClientStats[i].Email) == accountKey(email) {
					return &in.ClientStats[i]
				}
			}
		}
		return nil
	}

	ovpnRow := find(openvpn.Id)
	if ovpnRow == nil {
		t.Fatal("the openvpn inbound does not list the account")
	}
	if ovpnRow.Shared || ovpnRow.InboundDown != 4*mb {
		t.Errorf("openvpn row = (down %d, shared %v); want (%d, false)", ovpnRow.InboundDown, ovpnRow.Shared, 4*mb)
	}

	vlessRow := find(vless.Id)
	if vlessRow == nil {
		t.Fatal("the vless inbound does not list the account")
	}
	if !vlessRow.Shared {
		t.Error("the vless row must be marked shared: Xray's counter names no inbound, so " +
			"this figure cannot be claimed as that one inbound's own")
	}
	if vlessRow.InboundDown != 6*mb {
		t.Errorf("vless row inboundDown = %d; want the unattributed remainder %d, not a zero "+
			"(which reads as \"this customer never used it\")", vlessRow.InboundDown, 6*mb)
	}
}

// A panel whose accounts layer was never backfilled has no memberships to attribute
// with, and must render exactly as it did before: the whole account under its home
// inbound, nothing anywhere else.
func TestAttachClientStatsFallsBackWithoutMemberships(t *testing.T) {
	svc := newInboundDB(t)
	const email = "legacy@example.com"
	inbound := seedInboundWithClients(t, model.VLESS, 47301, []map[string]any{
		{"id": "3fa85f64-5717-4562-b3fc-2c963f66afa6", "email": email, "enable": true},
	})
	if err := database.GetDB().Create(&xray.ClientTraffic{
		InboundId: inbound.Id, Email: email, Enable: true, Up: 1 * mb, Down: 2 * mb, AllTime: 3 * mb,
	}).Error; err != nil {
		t.Fatalf("seed traffic: %v", err)
	}

	loaded, err := svc.GetAllInbounds()
	if err != nil {
		t.Fatalf("GetAllInbounds: %v", err)
	}
	if len(loaded) != 1 || len(loaded[0].ClientStats) != 1 {
		t.Fatalf("got %d inbounds with %d stats; want 1 and 1", len(loaded), len(loaded[0].ClientStats))
	}
	row := loaded[0].ClientStats[0]
	if row.InboundUp != 1*mb || row.InboundDown != 2*mb || row.InboundAllTime != 3*mb {
		t.Errorf("legacy row = (up %d, down %d, allTime %d); want the account's own "+
			"(%d, %d, %d) on its home inbound", row.InboundUp, row.InboundDown, row.InboundAllTime,
			1*mb, 2*mb, 3*mb)
	}
}
