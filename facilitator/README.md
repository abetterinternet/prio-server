# facilitator

This is ISRG's implementation of a Prio accumulation server. It ingests a share of data uploaded by a device, validates it against other facilitators, then emits the accumulated share to a Prio aggregator.

## Getting started

[Install a Rust toolchain](https://www.rust-lang.org/tools/install), then just `cargo build|run|test`. See `cargo run --bin facilitator -- --help` for information on the various options and subcommands.

## Generating ingestion data

To generate sample ingestion data, see the `generate-ingestion-sample` command and its usage (`cargo run --bin facilitator -- generate-ingestion-sample --help`).

## Docker

To build a Docker image, run `./build.sh`. To run that image locally, `docker run letsencrypt/prio-facilitator -- --help`.

## Linting manifest files

The `facilitator lint-manifest` subcommand can validate the various manifest files used in the system. See that subcommand's help text for more information on usage.

## Working with Avro files

If you want to examine Avro-encoded messages, you can use the `avro-tools` jar from the [Apache Avro project's releases](https://downloads.apache.org/avro/avro-1.10.0/java/), and then [use it from the command line to examine individual Avro encoded objects](https://www.michael-noll.com/blog/2013/03/17/reading-and-writing-avro-files-from-the-command-line/).

## References

[Prio Data Share Batch IDL](https://docs.google.com/document/d/1L06dpE7OcC4CXho2UswrfHrnWKtbA9aSSmO_5o7Ku6I/edit#heading=h.3kq1yexquq2g)
[ISRG Prio server-side components design doc](https://docs.google.com/document/d/1MdfM3QT63ISU70l63bwzTrxr93Z7Tv7EDjLfammzo6Q/edit#)
