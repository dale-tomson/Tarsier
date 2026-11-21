# Tarsier API Documentation

This directory contains the official **OpenAPI 3.1.0** specification for the Tarsier REST API:

- File: [`openapi.yaml`](openapi.yaml)
- Version: 0.1.1
- Base Path: `/api/v1`

---

## Importing into API Clients

### Postman
1. Open Postman.
2. Click **Import** in the upper left corner.
3. Select or drag-and-drop `docs/openapi.yaml`.
4. Choose **Postman Collection** with API definition.
5. In your collection settings:
   - Set variable `baseUrl` to `http://localhost:8080` (or your Tarsier instance URL).
   - Execute `POST /api/v1/auth/login` with your credentials (`username` and `password`) to set the `tarsier_session` cookie automatically.

### Bruno
1. Open Bruno.
2. Select **Import Collection** > **OpenAPI V3**.
3. Select `docs/openapi.yaml`.
4. Define your collection environment with `baseUrl: http://localhost:8080`.

### Insomnia
1. Open Insomnia.
2. Click **Import** > **File**.
3. Select `docs/openapi.yaml`.
4. Insomnia will generate a document and collection with all 34 REST endpoints.

---

## Interactive Local Documentation

### Using Swagger UI via Docker
Run a lightweight Swagger UI container pointing to your local specification:

```bash
docker run -p 8081:8080 -e SWAGGER_JSON=/spec/openapi.yaml -v $(pwd)/docs:/spec swaggerapi/swagger-ui
```
Open `http://localhost:8081` in your browser.

### Using VS Code / Cursor
Install the **OpenAPI (Swagger) Editor** or **Redocly** extension:
- Right-click `docs/openapi.yaml` and select **OpenAPI: Preview Documentation**.

---

## Authentication Notes

All endpoints under `/api/v1` (except `POST /api/v1/auth/login` and `/healthz`) require an active session.

1. **Authenticate**:
   ```bash
   curl -i -X POST http://localhost:8080/api/v1/auth/login \
     -H "Content-Type: application/json" \
     -d '{"username":"admin","password":"your-password"}' \
     -c cookies.txt
   ```
2. **Execute Authenticated Request**:
   ```bash
   curl -b cookies.txt http://localhost:8080/api/v1/system/health
   ```
