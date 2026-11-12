package testsupport_test

import (
	"testing"

	"github.com/JackMahfouz/e-wallet/services/wallet/internal/application"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/testsupport"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/testsupport/ledgercontract"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/testsupport/storecontract"
)

func TestMemStoreMeetsTheStoreContract(t *testing.T) {
	storecontract.Run(t, func(t *testing.T) application.Store { return testsupport.NewMemStore() })
}

func TestLedgerFakeMeetsTheLedgerContract(t *testing.T) {
	ledgercontract.Run(t, func(t *testing.T) application.Ledger { return &testsupport.Ledger{} })
}
