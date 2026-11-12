package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/JackMahfouz/e-wallet/services/wallet/internal/domain"
)

// Options tunes the rules. Zero values are not defaulted (a zero TimeoutWorkdays means "now");
// start from DefaultOptions.
type Options struct {
	// ConfirmTTL is how long an over-limit operation waits for its confirmation.
	ConfirmTTL time.Duration
	// MaxHoldTTL is the longest a hold may last.
	MaxHoldTTL time.Duration
	// TimeoutWorkdays is how many working days an operation has to succeed before it is failed.
	TimeoutWorkdays int
	// MaxAccounts is how many open (active or frozen) accounts a wallet may have.
	MaxAccounts int
	// FundingEnabled switches deposits and withdrawals on.
	FundingEnabled bool
	// RetryAfter is how long a new operation is left to the request that made it before recovery
	// may pick it up, and the basic length of a recovery claim.
	RetryAfter time.Duration
	// RecoveryBatch is how many operations one recovery pass handles.
	RecoveryBatch int
}

// DefaultOptions are the production defaults. Funding is off.
func DefaultOptions() Options {
	return Options{
		ConfirmTTL:      10 * time.Minute,
		MaxHoldTTL:      7 * 24 * time.Hour,
		TimeoutWorkdays: 5,
		MaxAccounts:     10,
		RetryAfter:      30 * time.Second,
		RecoveryBatch:   50,
	}
}

// Dependencies are the outbound ports the Service needs.
type Dependencies struct {
	Store   Store
	Ledger  Ledger
	Tokens  TokenVerifier
	OTP     OTPVerifier
	Funding FundingGateway
	Clock   Clock
	IDs     IDGenerator
	Logger  *slog.Logger
}

// Service implements the use cases. It is safe for concurrent use.
type Service struct {
	Dependencies
	opts Options
}

var (
	_ Wallets  = (*Service)(nil)
	_ Internal = (*Service)(nil)
	_ Recovery = (*Service)(nil)
)

// New returns a Service over deps.
func New(deps Dependencies, opts Options) *Service {
	if deps.Logger == nil {
		deps.Logger = slog.New(slog.DiscardHandler)
	}

	return &Service{Dependencies: deps, opts: opts}
}

// Authenticate resolves a bearer token to a user id.
func (s *Service) Authenticate(ctx context.Context, bearerToken string) (string, error) {
	if strings.TrimSpace(bearerToken) == "" {
		return "", domain.ErrUnauthenticated
	}

	subject, err := s.Tokens.Verify(ctx, bearerToken)
	if err != nil {
		return "", err
	}

	userID, err := domain.ParseID("subject", subject)
	if err != nil {
		// A token whose subject is not a user id (an old integer id, say) is not ours to honour.
		return "", domain.ErrUnauthenticated
	}

	return userID, nil
}

// requireOTP checks the one-time code for a sensitive action.
func (s *Service) requireOTP(ctx context.Context, userID, code string) error {
	return s.OTP.Verify(ctx, userID, code)
}

// ensureWallet returns the user's wallet, making it on first use.
func (s *Service) ensureWallet(ctx context.Context, repo Repository, userID string) (domain.Wallet, error) {
	now := s.Clock.Now()

	return repo.EnsureWallet(ctx, domain.Wallet{ID: s.IDs.NewID(), UserID: userID, CreatedAt: now, UpdatedAt: now})
}

// ownedAccount loads an account and checks it belongs to the user. Another user's account is
// reported as not found, never as forbidden, so ids cannot be probed.
func (s *Service) ownedAccount(ctx context.Context, repo Repository, userID, accountID string) (domain.Wallet, domain.Account, error) {
	id, err := domain.ParseID("account id", accountID)
	if err != nil {
		return domain.Wallet{}, domain.Account{}, err
	}

	wallet, err := repo.WalletByUser(ctx, userID)
	if err != nil {
		return domain.Wallet{}, domain.Account{}, err
	}

	account, err := repo.AccountByID(ctx, id)
	if err != nil {
		return domain.Wallet{}, domain.Account{}, err
	}
	if account.WalletID != wallet.ID {
		return domain.Wallet{}, domain.Account{}, domain.ErrNotFound
	}

	return wallet, account, nil
}

// available is what the account can spend right now. The order of the reads matters: the
// operations and holds first, the ledger balance last. An outgoing operation that moves from
// pending to debited between the two reads is then counted twice (the safe way to be wrong)
// instead of not at all.
func (s *Service) available(ctx context.Context, repo Repository, accountID string, now time.Time) (int64, error) {
	view, err := s.accountFigures(ctx, repo, accountID, now)

	return view.Available, err
}

func (s *Service) accountFigures(ctx context.Context, repo Repository, accountID string, now time.Time) (AccountView, error) {
	held, err := repo.ActiveHeld(ctx, accountID, now)
	if err != nil {
		return AccountView{}, err
	}

	pending, err := repo.PendingOutgoing(ctx, accountID)
	if err != nil {
		return AccountView{}, err
	}

	balance, err := s.Ledger.Balance(ctx, accountID)
	if err != nil {
		return AccountView{}, err
	}

	return AccountView{
		Balance:   balance,
		Held:      held,
		Available: domain.Available(balance, held, pending),
	}, nil
}

// view builds the figures for one account.
func (s *Service) view(ctx context.Context, repo Repository, account domain.Account, now time.Time) (AccountView, error) {
	view, err := s.accountFigures(ctx, repo, account.ID, now)
	if err != nil {
		return AccountView{}, err
	}

	spent, err := repo.SpentSince(ctx, account.ID, domain.StartOfDay(now))
	if err != nil {
		return AccountView{}, err
	}

	view.Account = account
	view.SpentToday = spent

	return view, nil
}

// requestHash fingerprints a request, so an idempotency key reused for a different request is
// caught.
func requestHash(parts ...any) string {
	var b strings.Builder
	for _, part := range parts {
		fmt.Fprintf(&b, "%v|", part)
	}

	sum := sha256.Sum256([]byte(b.String()))

	return hex.EncodeToString(sum[:])
}

// serviceInitiator validates the name of a calling service and returns the initiator string.
func serviceInitiator(name string) (string, error) {
	if len(name) == 0 || len(name) > 32 {
		return "", domain.Invalidf("service must be 1 to 32 characters")
	}
	for i, r := range name {
		letter := r >= 'a' && r <= 'z'
		digit := r >= '0' && r <= '9'
		if !(letter || (i > 0 && (digit || r == '-' || r == '_'))) {
			return "", domain.Invalidf("service must start with a lower-case letter and use only a-z, 0-9, '-' and '_'")
		}
	}

	return "service:" + name, nil
}

func userInitiator(userID string) string { return "user:" + userID }

// isTransient reports whether err is a dependency being unreachable, which is worth retrying.
func isTransient(err error) bool { return errors.Is(err, domain.ErrUnavailable) }
