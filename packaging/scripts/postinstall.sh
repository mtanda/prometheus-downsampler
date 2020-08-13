systemctl daemon-reload
systemctl enable prometheus-downsampler.service
systemctl restart prometheus-downsampler.service
chown nobody:nogroup /var/tmp/prometheus-downsampler