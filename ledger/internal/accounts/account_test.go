package accounts

import (
	"errors"
	"testing"
)

func TestNormalSideFor(t *testing.T) {
	cases := []struct {
		typ  Type
		want int16
	}{
		{Asset, 1},
		{Expense, 1},
		{Liability, -1},
		{Revenue, -1},
		{Equity, -1},
		{Position, 0},
	}
	for _, c := range cases {
		got, err := NormalSideFor(c.typ)
		if err != nil {
			t.Fatalf("NormalSideFor(%s): unexpected error: %v", c.typ, err)
		}
		if got != c.want {
			t.Errorf("NormalSideFor(%s) = %d, want %d", c.typ, got, c.want)
		}
		if !c.typ.Valid() {
			t.Errorf("%s.Valid() = false, want true", c.typ)
		}
	}

	_, err := NormalSideFor(Type("BOGUS"))
	if !errors.Is(err, ErrUnknownAccountType) {
		t.Errorf("NormalSideFor(BOGUS): got %v, want ErrUnknownAccountType", err)
	}
	if Type("BOGUS").Valid() {
		t.Error("Type(\"BOGUS\").Valid() = true, want false")
	}
}
