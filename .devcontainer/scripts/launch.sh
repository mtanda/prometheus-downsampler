#!/bin/sh

./prometheus-downsampler --prometheus.addr=http://prometheus:9090/ --tsdb.tmp-path=./data.tmp --tsdb.path=./data --config.file=./prometheus-downsampler.yml
