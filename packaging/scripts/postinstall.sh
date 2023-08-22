systemctl daemon-reload
systemctl enable prometheus-downsampler.timer
systemctl start prometheus-downsampler.timer
chown nobody:nogroup /var/tmp/prometheus-downsampler
