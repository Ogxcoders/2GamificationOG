# admin console (Vite dev server for local; `vite build` for prod statics).
FROM node:22-alpine
WORKDIR /app
COPY apps/admin/package.json ./
RUN npm install --no-audit --no-fund
COPY apps/admin ./
EXPOSE 5173
CMD ["npx", "vite", "--host", "0.0.0.0"]
