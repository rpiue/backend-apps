# ---------------------------------------------------------------------------
# Build estático del backend Go.
#
# Seguridad de la imagen final:
#   · Se compila estático (CGO_ENABLED=0) y se ejecuta sobre una base mínima.
#   · Corre como usuario SIN privilegios: si alguien logra ejecución de código
#     dentro del contenedor, no es root y no puede escribir en el sistema de
#     archivos ni escalar con capacidades del kernel.
#   · -trimpath y -ldflags="-s -w" quitan las rutas de compilación y la tabla de
#     símbolos: menos información regalada a quien analice el binario.
# ---------------------------------------------------------------------------
FROM golang:1.26-alpine AS build

# git es necesario si alguna dependencia se resuelve por VCS; ca-certificates
# para que `go mod download` valide TLS contra el proxy de módulos.
RUN apk add --no-cache ca-certificates git

WORKDIR /src

# Capa de dependencias separada: solo se reconstruye si cambian go.mod/go.sum.
COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY . .

# GOFLAGS=-mod=readonly: el build falla si el código pide una dependencia que no
# está en go.mod, en vez de descargarla en silencio (defensa básica de cadena
# de suministro: lo que se compila es exactamente lo que dice go.sum).
ENV CGO_ENABLED=0 GOOS=linux GOFLAGS=-mod=readonly
RUN go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# ---------------------------------------------------------------------------
FROM alpine:3.20

# ca-certificates es OBLIGATORIO: sin el almacén de CA raíz, la verificación de
# certificados TLS de las llamadas salientes (MercadoPago, BTCPay, Firebase)
# falla y la tentación es "arreglarlo" desactivando la verificación, que es
# justo lo que abre la puerta al MitM. tzdata para America/Lima.
RUN apk add --no-cache ca-certificates tzdata wget && \
    addgroup -g 10001 -S app && \
    adduser  -u 10001 -S app -G app

WORKDIR /app
COPY --from=build --chown=root:root --chmod=0555 /out/server /app/server

# Puntos de montaje que el proceso necesita escribir (adjuntos del chat e
# imágenes públicas). Se crean con el dueño correcto para que el usuario sin
# privilegios pueda escribir en ellos.
RUN mkdir -p /app/chat-uploads /app/uploads /app/secrets && \
    chown -R app:app /app/chat-uploads /app/uploads

USER app:app

EXPOSE 3001

# El health check usa la ruta /codex, que ForceHTTPS deja pasar sin TLS a
# propósito para que el sondeo interno funcione sin certificados.
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD wget -qO- --tries=1 --timeout=4 http://127.0.0.1:3001/codex || exit 1

ENTRYPOINT ["/app/server"]
