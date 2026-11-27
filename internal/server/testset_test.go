package server

import "github.com/omacom/omarchy-maclab/internal/agent/tests/bench"

// testSet is the Benchmark set the lab runs, which the tests' values are of.
var testSet = func() string {
	s, err := bench.Set()
	if err != nil {
		panic(err)
	}
	return s.Name
}()
