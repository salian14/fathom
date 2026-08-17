FROM python:3.12-slim AS build

WORKDIR /src

# Dependencies first: this layer is the expensive one and changes far less often
# than the source does.
COPY requirements.lock ./
RUN python -m venv /opt/venv \
    && /opt/venv/bin/pip install --no-cache-dir -r requirements.lock

COPY pyproject.toml ./
COPY src/ src/
RUN /opt/venv/bin/pip install --no-cache-dir --no-deps .


FROM python:3.12-slim
# Distroless is not workable here the way it is for the Go component: that base is
# for static binaries, and this dependency tree needs a real interpreter and its
# shared libraries.

COPY --from=build /opt/venv /opt/venv

ENV PATH="/opt/venv/bin:$PATH" \
    PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    # The chart runs this with readOnlyRootFilesystem, so anything that wants to
    # write must be pointed at the /tmp emptyDir.
    HOME=/tmp

ARG VERSION=dev
LABEL org.opencontainers.image.title="fathom-agent" \
      org.opencontainers.image.version="${VERSION}"

USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["python", "-m", "fathom_agent"]
