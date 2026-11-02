FROM busybox:1.37.0@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec
COPY index.html /www/index.html
# A third-party script the image ships, which no scanner sees in it, and the SBOM document that
# declares it: the image's SBOM merges the document.
COPY vendor/ /www/vendor/
COPY sbom.cdx.json /usr/share/sbom/ui.cdx.json
USER 101
EXPOSE 8080
CMD ["httpd", "-f", "-p", "8080", "-h", "/www"]
