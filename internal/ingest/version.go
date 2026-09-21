package ingest

// SplitterVersion identifies the evidence ingestion algorithm (block parsing,
// ID assignment, and kind mapping) that produced a set of evidence units.
//
// It is a plain monotonic identifier, not semver or a hash. Bump it by editing
// this constant only when the ingestion algorithm's output changes for the same
// input. It is metadata recorded beside a snapshot; it is NOT part of the
// snapshot content hash.
const SplitterVersion = "1"

// Splitter returns the current splitter version. Prefer this accessor at call
// sites so the constant has a single named entry point.
//
// It is pure: it takes no arguments, consults no clock, filesystem, network, or
// random source, and always returns the non-empty SplitterVersion.
func Splitter() string {
	return SplitterVersion
}
