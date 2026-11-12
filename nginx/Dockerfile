FROM nginx:1.27-alpine

LABEL isola.stack="1"

COPY nginx.conf /etc/nginx/nginx.conf

EXPOSE 80

HEALTHCHECK --interval=10s --timeout=3s --retries=3 \
    CMD curl -f http://localhost:80/health || exit 1
