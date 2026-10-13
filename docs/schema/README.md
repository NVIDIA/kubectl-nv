# Generated output schemas

This directory holds one JSON Schema per kubectl-nv output kind, generated from
the Go type the command marshals. Do not edit the files by hand: run `make
schema` and commit what it writes. A test fails when a checked-in schema
differs from what the generator produces.

A consumer validates what kubectl-nv printed against the file here, so these
files are the published contract rather than a convenience copy of it.
