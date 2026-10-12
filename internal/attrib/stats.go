package attrib

import "math"

// signTail is the exact lower tail of a fair-coin sign test: P(X <= k) for
// X ~ Binomial(n, 0.5).
//
// The exact distribution is used rather than a normal approximation because the
// sample sizes here are small -- a dataset of eight tasks is a normal dataset --
// and the normal approximation is at its worst exactly there. Its failure mode
// is the one that matters: it reports significance for splits that cannot
// support it.
//
// The terms are summed in log space. 0.5^n underflows a float64 at n = 1074,
// and the binomial coefficient overflows far earlier, so computing either
// directly would produce a confident zero for a sweep with a thousand tasks.
func signTail(n, k int) float64 {
	if k < 0 {
		return 0
	}
	if k >= n {
		return 1
	}
	sum := 0.0
	for i := 0; i <= k; i++ {
		sum += math.Exp(logChoose(n, i) - float64(n)*math.Ln2)
	}
	return math.Min(1, sum)
}

// logChoose is ln C(n, k), computed through lgamma so that neither the
// factorial nor the coefficient has to be representable.
func logChoose(n, k int) float64 {
	if k < 0 || k > n {
		return math.Inf(-1)
	}
	return lnGamma(n+1) - lnGamma(k+1) - lnGamma(n-k+1)
}

// lnGamma is ln Γ(x). math.Lgamma also returns the sign of Γ, which is
// discarded: every argument passed here is an integer plus one, so it is at
// least 1, and Γ is positive for all of those.
func lnGamma(x int) float64 {
	v, _ := math.Lgamma(float64(x))
	return v
}
