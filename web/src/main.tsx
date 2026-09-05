import { StrictMode, useEffect, useState } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter, Navigate, Route, Routes } from "react-router-dom";

import "./index.css";
import { Layout } from "./components/Layout";
import { getToken, applyTheme } from "./lib/api";
import { AccountsPage } from "./pages/Accounts";
import { ComposePage } from "./pages/Compose";
import { DashboardPage } from "./pages/Dashboard";
import { LoginPage } from "./pages/Login";
import { SendDetailPage } from "./pages/SendDetail";
import { SendsPage } from "./pages/Sends";

// Apply the saved theme before first paint to avoid a flash.
applyTheme((localStorage.getItem("carteiro.theme") as "dark" | "light" | null) ?? "dark");

function RequireAuth({ children }: { children: React.ReactNode }) {
  if (!getToken()) return <Navigate to="/login" replace />;
  return <>{children}</>;
}

function AppRoutes() {
  // Re-render when the session is cleared anywhere (401 handler).
  const [, bump] = useState(0);
  useEffect(() => {
    const onUnauthorized = () => bump((n) => n + 1);
    window.addEventListener("carteiro:unauthorized", onUnauthorized);
    return () => window.removeEventListener("carteiro:unauthorized", onUnauthorized);
  }, []);

  return (
    <Routes>
      <Route path="/login" element={<LoginPage />} />
      <Route
        element={
          <RequireAuth>
            <Layout />
          </RequireAuth>
        }
      >
        <Route path="/" element={<DashboardPage />} />
        <Route path="/compose" element={<ComposePage />} />
        <Route path="/sends" element={<SendsPage />} />
        <Route path="/sends/:id" element={<SendDetailPage />} />
        <Route path="/accounts" element={<AccountsPage />} />
        <Route path="*" element={<Navigate to="/" replace />} />
      </Route>
    </Routes>
  );
}

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <BrowserRouter>
      <AppRoutes />
    </BrowserRouter>
  </StrictMode>,
);
