package oauth

import (
	"testing"

	"github.com/smeltdev/smelt/internal/config/configtest"
)

// TestMain keeps these tests away from the user's real smelt data.
func TestMain(m *testing.M) { configtest.Main(m) }
