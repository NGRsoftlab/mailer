// Package fuzzing_tests contains fuzz tests for the mailer library.
//
// Fuzz targets are gated behind the "fuzz" build tag so that the regular
// test suite does not pay for the seed corpus on every run. To run them
// locally:
//
//	go test -tags=fuzz ./fuzzing_tests/
//	go test -tags=fuzz -run=^$ -fuzz=FuzzMessageRender -fuzztime=60s ./fuzzing_tests/
package fuzzing_tests
