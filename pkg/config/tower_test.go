package config

import "testing"

func TestApprenticeConfig_EffectiveTransport(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty defaults to push", in: "", want: ApprenticeTransportPush},
		{name: "push explicit", in: ApprenticeTransportPush, want: ApprenticeTransportPush},
		{name: "bundle explicit", in: ApprenticeTransportBundle, want: ApprenticeTransportBundle},
		{name: "arbitrary value passes through", in: "carrier-pigeon", want: "carrier-pigeon"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := ApprenticeConfig{Transport: tc.in}
			if got := c.EffectiveTransport(); got != tc.want {
				t.Fatalf("EffectiveTransport() = %q, want %q", got, tc.want)
			}
		})
	}
}
