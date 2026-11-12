package funding_test

import (
	"context"
	"errors"
	"testing"

	"github.com/JackMahfouz/e-wallet/services/wallet/internal/adapter/out/funding"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/domain"
)

func TestSimulatedFundingAlwaysSucceedsAndRepeats(t *testing.T) {
	var gateway funding.Simulated

	for i := 0; i < 2; i++ { // a repeat with the same reference is fine
		if err := gateway.Collect(context.Background(), "ref", 100); err != nil {
			t.Errorf("Collect: %v", err)
		}
		if err := gateway.Payout(context.Background(), "ref", 100); err != nil {
			t.Errorf("Payout: %v", err)
		}
	}
}

func TestRefusingFundingRefusesEverythingForGood(t *testing.T) {
	var gateway funding.Refusing

	if err := gateway.Collect(context.Background(), "ref", 100); !errors.Is(err, domain.ErrRejected) {
		t.Errorf("Collect: %v, want ErrRejected", err)
	}
	if err := gateway.Payout(context.Background(), "ref", 100); !errors.Is(err, domain.ErrRejected) {
		t.Errorf("Payout: %v, want ErrRejected", err)
	}
}
