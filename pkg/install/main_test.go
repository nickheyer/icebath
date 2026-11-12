package install

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Child mode holding an install's checkout lock until stdin closes, the foreign holder the lock cases need
const holdCheckoutEnv = "ICEBATH_INSTALL_TEST_HOLD_CHECKOUT"

// Child mode that just stays alive, the foreign live updater the marker cases need
const sleepEnv = "ICEBATH_INSTALL_TEST_SLEEP"

func TestMain(m *testing.M) {
	if os.Getenv(sleepEnv) != "" {
		time.Sleep(60 * time.Second)
		os.Exit(0)
	}
	if root := os.Getenv(holdCheckoutEnv); root != "" {
		if _, err := updaterecord.AcquireCheckout(root); err != nil {
			fmt.Println("error", err)
			os.Exit(1)
		}
		fmt.Println("ready")
		bufio.NewReader(os.Stdin).ReadString('\n')
		os.Exit(0)
	}
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	home.DispatchScratchPruneEntry(context.Background())
	os.Exit(m.Run())
}
