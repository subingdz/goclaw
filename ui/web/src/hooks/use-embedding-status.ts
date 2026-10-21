import { useState, useEffect, useCallback } from "react";
import { useHttp } from "./use-ws";
import { useAuthStore } from "@/stores/use-auth-store";

interface EmbeddingStatus {
  configured: boolean;
  provider?: string;
  provider_name?: string;
  model?: string;
  // Width the memory vector columns actually declare, read from the database catalog.
  // 0 = this backend stores no vectors (desktop/SQLite).
  schema_dimensions?: number;
  max_indexable_dimensions?: number;
}

export function useEmbeddingStatus() {
  const http = useHttp();
  const tenantId = useAuthStore((s) => s.tenantId);
  const [status, setStatus] = useState<EmbeddingStatus | null>(null);
  const [loading, setLoading] = useState(true);

  const refresh = useCallback(async () => {
    try {
      const res = await http.get<EmbeddingStatus>("/v1/embedding/status");
      setStatus(res);
    } catch {
      setStatus({ configured: false });
    } finally {
      setLoading(false);
    }
  }, [http]);

  // Re-fetch when tenant changes
  useEffect(() => { refresh(); }, [refresh, tenantId]);

  return { status, loading, refresh };
}
