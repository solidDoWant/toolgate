// Package testtime shortens the gateway's fixed waits and minimum durations
// in end-to-end tests.
//
// Scale returns its argument unchanged in every normal build, including the
// release image. Only a binary built with the e2e build tag reads
// TOOLGATE_TEST_TIME_SCALE and multiplies durations by it, so that e2e tests
// do not wait out minutes of real product time. The e2e test harness builds
// such a gateway image and sets the variable; the chart never sets it.
package testtime

// EnvVar is the environment variable an e2e build reads the scale from: a
// positive decimal number such as 0.1.
const EnvVar = "TOOLGATE_TEST_TIME_SCALE"
