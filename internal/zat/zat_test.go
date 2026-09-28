package zat_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/valargroup/valar-testnet-faucet/internal/zat"
)

func TestFormatAndDisplay(t *testing.T) {
	tests := []struct {
		z       int64
		format  string
		display string
	}{
		{0, "0.00000000", "0"},
		{1, "0.00000001", "0.00000001"},
		{12_500_000, "0.12500000", "0.125"},
		{100_000_000, "1.00000000", "1"},
		{1_250_000_000, "12.50000000", "12.5"},
		{-12_500_000, "-0.12500000", "-0.125"},
	}
	for _, tt := range tests {
		require.Equal(t, tt.format, zat.Format(tt.z))
		require.Equal(t, tt.display, zat.Display(tt.z))
	}
}

func TestParse(t *testing.T) {
	tests := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{in: "0", want: 0},
		{in: "0.125", want: 12_500_000},
		{in: "0.12500000", want: 12_500_000},
		{in: "12.5", want: 1_250_000_000},
		{in: "0.00000001", want: 1},
		{in: "-0.5", want: -50_000_000},
		{in: "92233720368.54775807", want: 1<<63 - 1},
		{in: "", wantErr: true},
		{in: ".5", wantErr: true},
		{in: "1.", wantErr: true},
		{in: "0.000000001", wantErr: true},
		{in: "1e-3", wantErr: true},
		{in: " 1", wantErr: true},
		{in: "1,5", wantErr: true},
		{in: "92233720368.54775808", wantErr: true},
		{in: "99999999999999999999", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := zat.Parse(tt.in)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}
