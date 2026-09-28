package faucet

import (
	"context"

	"github.com/valargroup/valar-testnet-faucet/internal/store"
)

// Pay exposes pay for tests.
func (s *Service) Pay(ctx context.Context, c store.Claim) bool { return s.pay(ctx, c) }
