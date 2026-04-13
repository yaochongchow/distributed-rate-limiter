FROM node:22-alpine
WORKDIR /app

# Dependencies first for layer caching
COPY package.json ./
RUN npm install --omit=dev

COPY server.js ./
COPY public/ ./public/

EXPOSE 4000
CMD ["node", "server.js"]
