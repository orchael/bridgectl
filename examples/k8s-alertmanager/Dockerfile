FROM ghcr.io/orchael/bridgectl@sha256:59f8c36452f1c4b2b2809a99560b97e91721d00053235ad98d837d07099f5508
COPY --chown=bridge:bridge examples/k8s-alertmanager/run.mjs /app/run.mjs
COPY --chown=bridge:bridge examples/k8s-alertmanager/snapshot.mjs /app/snapshot.mjs
COPY --chown=bridge:bridge examples/k8s-alertmanager/alerts.mjs /app/alerts.mjs
USER bridge
