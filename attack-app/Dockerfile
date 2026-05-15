FROM node:20-alpine AS builder

WORKDIR /app

COPY package.json ./
RUN npm install --omit=dev

COPY src/ ./src/
COPY miner-stub.js ./miner-stub.js

FROM node:20-alpine

WORKDIR /app

COPY --from=builder /app .

# Install miner-stub as an executable in PATH so execFile can find it by path.
RUN cp /app/miner-stub.js /usr/local/bin/miner-stub && \
    chmod +x /usr/local/bin/miner-stub

EXPOSE 3000

USER node

CMD ["node", "-r", "/app/src/tracing.js", "/app/src/index.js"]
