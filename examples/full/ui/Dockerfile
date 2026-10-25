FROM nginxinc/nginx-unprivileged:1.27-alpine@sha256:65e3e85dbaed8ba248841d9d58a899b6197106c23cb0ff1a132b7bfe0547e4c0
COPY index.html /usr/share/nginx/html/index.html
# A third-party script the image ships, which no scanner sees in it, and the SBOM document that
# declares it: the image's SBOM merges the document.
COPY vendor/ /usr/share/nginx/html/vendor/
COPY sbom.cdx.json /usr/share/sbom/ui.cdx.json
