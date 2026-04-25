package fake_test

import (
	"testing"

	"github.com/awell-health/spire/pkg/wizardregistry"
	"github.com/awell-health/spire/pkg/wizardregistry/conformance"
	"github.com/awell-health/spire/pkg/wizardregistry/fake"
)

func TestFakeConformance(t *testing.T) {
	conformance.Run(t, func(t *testing.T) (wizardregistry.Registry, conformance.Control) {
		r := fake.New()
		return r, r
	})
}
