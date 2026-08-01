package xray

import (
	"strings"
	"testing"
)

// Init is allowed to fail (Xray not running, API port 0, dial refused) and every
// caller in web/service ignores that error, so these four can be reached with no
// client at all.
//
// They used to dereference *x.HandlerServiceClient straight away. That does not
// return an error, it panics, and it panics on the request goroutine, so the
// whole PANEL process dies. Worse, it is reachable in exactly the situation an
// operator is most likely to be clicking around in: a core that refused its
// config and is not up. Adding an inbound then killed the panel instead of
// reporting that Xray was unreachable.
func TestHandlerMethodsReportInsteadOfPanicWhenNotConnected(t *testing.T) {
	cases := []struct {
		name string
		call func(x *XrayAPI) error
	}{
		{"AddInbound", func(x *XrayAPI) error { return x.AddInbound([]byte(`{"tag":"t"}`)) }},
		{"DelInbound", func(x *XrayAPI) error { return x.DelInbound("t") }},
		{"AddUser", func(x *XrayAPI) error {
			return x.AddUser("vmess", "t", map[string]any{"email": "e", "id": "i"})
		}},
		{"RemoveUser", func(x *XrayAPI) error { return x.RemoveUser("t", "e") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("%s PANICKED with no API connection (%v): this kills the panel process", tc.name, r)
				}
			}()
			x := &XrayAPI{} // never Init'd, exactly as after a failed connect
			err := tc.call(x)
			if err == nil {
				t.Fatalf("%s returned no error with no API connection", tc.name)
			}
			if !strings.Contains(err.Error(), "not connected") {
				t.Errorf("%s error should say the api is not connected; got: %v", tc.name, err)
			}
		})
	}
}

// Init must refuse a port it cannot dial rather than leaving a half-built client
// behind, which is the state the guards above exist to catch.
func TestInitRefusesInvalidPort(t *testing.T) {
	x := &XrayAPI{}
	if err := x.Init(0); err == nil {
		t.Fatal("Init(0) returned no error")
	}
	if x.HandlerServiceClient != nil {
		t.Error("Init left a handler client behind after refusing")
	}
}
