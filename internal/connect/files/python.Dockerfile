# Proposed by Forge for this Python project. Forge runs it as a non-root user
# with a read-only root filesystem: only /tmp is writable. The app listens on $PORT.
FROM mirror.gcr.io/library/python:3.13-slim
WORKDIR /app
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 HOME=/tmp
COPY . .
RUN {% .PipInstall %}
USER 1000:1000
CMD ["python", "{% .Entry %}"]
