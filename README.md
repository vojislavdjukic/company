# company
Hire and chat with your AI employees

## Development

The Vue app lives in `frontend/`. Install its dependencies once, then start
both Vite (with browser HMR) and the Go API under Air:

```sh
npm --prefix frontend install
./dev.sh
```

Open <http://127.0.0.1:5173>. Vite proxies API requests to the Go server on
port 4000. Stop `dev.sh` with Ctrl+C; it shuts down Vite and Air, and Air
stops the Go process it started.

For a production build, run `npm --prefix frontend run build` and then build
the Go backend from `backend/`. The built Vue files are embedded in the Go
binary, which serves both the frontend and API from one process.
