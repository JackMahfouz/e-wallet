// Package ledger is the driven adapter that talks to the ledger service over gRPC. It implements
// application.Ledger, turning cents into the ledger's decimal amounts (and back) with the shared
// money package, and the ledger's status codes into the errors the core understands.
package ledger

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"time"

	processmoney "github.com/JackMahfouz/e-wallet/libs/process-money"
	ledgerv1 "github.com/JackMahfouz/e-wallet/services/ledger/gen/ledger/v1"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/application"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/domain"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Client is the ledger as the wallet's core sees it.
type Client struct {
	ledger  ledgerv1.LedgerClient
	timeout time.Duration
}

var _ application.Ledger = (*Client)(nil)

// New returns a Client that gives each call at most timeout.
func New(ledger ledgerv1.LedgerClient, timeout time.Duration) *Client {
	return &Client{ledger: ledger, timeout: timeout}
}

// Balance implements application.Ledger.
func (c *Client) Balance(ctx context.Context, accountID string) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	resp, err := c.ledger.GetBalance(ctx, &ledgerv1.GetBalanceRequest{AccountId: accountID})
	if err != nil {
		return 0, translate("balance", err, 0)
	}

	cents, err := processmoney.BalanceToMinorUnits(resp.GetBalance())
	if err != nil {
		return 0, fmt.Errorf("%w: the ledger returned a balance of %v: %v", domain.ErrRejected, resp.GetBalance(), err)
	}

	return cents, nil
}

// Debit implements application.Ledger.
func (c *Client) Debit(ctx context.Context, accountID, transactionID string, cents int64) (application.LedgerPosting, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	resp, err := c.ledger.Debit(ctx, post(accountID, transactionID, cents))
	if err != nil {
		return application.LedgerPosting{}, translate("debit", err, cents)
	}

	return application.LedgerPosting{Replayed: resp.GetReplayed()}, nil
}

// Credit implements application.Ledger.
func (c *Client) Credit(ctx context.Context, accountID, transactionID string, cents int64) (application.LedgerPosting, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	resp, err := c.ledger.Credit(ctx, post(accountID, transactionID, cents))
	if err != nil {
		return application.LedgerPosting{}, translate("credit", err, cents)
	}

	return application.LedgerPosting{Replayed: resp.GetReplayed()}, nil
}

func post(accountID, transactionID string, cents int64) *ledgerv1.PostRequest {
	return &ledgerv1.PostRequest{
		AccountId:     accountID,
		TransactionId: transactionID,
		Amount:        processmoney.FromMinorUnits(cents),
	}
}

// balanceInMessage picks the balance out of the ledger's "insufficient funds: balance 12.50,
// requested 20.00" message, so the caller can be told what was available.
var balanceInMessage = regexp.MustCompile(`balance (-?\d+(?:\.\d+)?)`)

// translate maps a gRPC failure to the core's errors. A refusal that will not change (a bad id, an
// amount the ledger rejects, a transaction id reused with another amount) is ErrRejected; a missing
// answer (the ledger down, slow, or failing) is ErrUnavailable, because the call may or may not
// have been applied and the only safe thing to do is ask again.
func translate(doing string, err error, requested int64) error {
	st, ok := status.FromError(err)
	if !ok {
		return fmt.Errorf("%w: ledger %s: %v", domain.ErrUnavailable, doing, err)
	}

	switch st.Code() {
	case codes.FailedPrecondition:
		insufficient := &domain.InsufficientFundsError{Requested: requested}
		if m := balanceInMessage.FindStringSubmatch(st.Message()); m != nil {
			if balance, err := strconv.ParseFloat(m[1], 64); err == nil {
				if cents, err := processmoney.BalanceToMinorUnits(balance); err == nil {
					insufficient.Available = cents
				}
			}
		}

		return insufficient
	case codes.InvalidArgument, codes.AlreadyExists, codes.NotFound, codes.PermissionDenied, codes.Unauthenticated:
		return fmt.Errorf("%w: ledger %s: %s", domain.ErrRejected, doing, st.Message())
	default:
		return fmt.Errorf("%w: ledger %s: %s: %s", domain.ErrUnavailable, doing, st.Code(), st.Message())
	}
}
