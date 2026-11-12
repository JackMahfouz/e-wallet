package application

import (
	"context"

	"github.com/JackMahfouz/e-wallet/services/wallet/internal/domain"
)

// AdvanceForTest lets the tests drive an operation from a stale copy of it, which is what a
// request does when recovery has already moved the operation on. Compiled only into tests.
func (s *Service) AdvanceForTest(ctx context.Context, op domain.Operation) domain.Operation {
	return s.advance(ctx, op)
}

// ExpireConfirmationForTest tries to expire an awaiting operation, as the recovery job does for
// each one it finds due. It reports whether the operation was failed.
func (s *Service) ExpireConfirmationForTest(ctx context.Context, op domain.Operation) bool {
	return s.expireConfirmation(ctx, op)
}
