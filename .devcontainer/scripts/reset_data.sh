#!/bin/sh

cd `dirname $0`
sudo sudo -u nobody rm -rf /prometheus/*
curl -X PUT 'http://prometheus:9090/-/quit' && echo
while [ `curl 'http://prometheus:9090/-/ready' -s -o /dev/null -w '%{http_code}\n'` != 200 ]; do
  sleep 1
done
rm -f /tmp/metrics.dat
go run gen_test_data.go > /tmp/metrics.dat
sudo sudo -u nobody promtool tsdb create-blocks-from openmetrics /tmp/metrics.dat /prometheus
curl -X PUT 'http://prometheus:9090/-/quit' && echo
while [ `curl 'http://prometheus:9090/-/ready' -s -o /dev/null -w '%{http_code}\n'` != 200 ]; do
  sleep 1
done
