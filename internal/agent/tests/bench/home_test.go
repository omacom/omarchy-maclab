package bench

import (
	"testing"

	"github.com/omacom/omarchy-maclab/internal/testhome"
)

// No test here reads or writes the home of whoever runs the tests.
func TestMain(m *testing.M) {
	testhome.Main(m)
}
