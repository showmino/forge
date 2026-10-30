# Proposed by Forge for this Node project. Forge runs it as a non-root user with
# a read-only root filesystem: only /tmp is writable, so HOME and npm's cache
# are there. The app listens on $PORT.
FROM mirror.gcr.io/library/node:24-slim
WORKDIR /app
COPY package*.json ./
RUN {% .Install %}
COPY . .
{%- if .Build %}
RUN {% .Build %}
{%- end %}
ENV NODE_ENV=production HOME=/tmp npm_config_cache=/tmp/.npm
USER 1000:1000
CMD {% .Cmd %}
