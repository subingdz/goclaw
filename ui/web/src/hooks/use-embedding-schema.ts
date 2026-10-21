import { useCallback, useEffect, useState } from "react";
import { useHttp } from "./use-ws";

export interface EmbeddingSchemaColumn {
  table: string;
  column: string;
  type_mod: string;
  dimensions: number;
  embedded: number;
  indexes?: string[];
  error?: string;
}

export interface EmbeddingSchema {
  schema_dimensions: number;
  max_indexable_dimensions: number;
  columns: EmbeddingSchemaColumn[];
  error?: string;
}

export interface RedimensionColumn {
  table: string;
  column: string;
  from: number;
  to: number;
  applied: boolean;
  action: string;
  indexes?: string[];
  error?: string;
}

export interface RedimensionReport {
  target: number;
  columns: RedimensionColumn[];
  embedded_rows: number;
  needs_force: boolean;
  unreadable: number;
  error?: string;
}

/** The stored vector width lives in the database, not in config, so both the read and the
 * re-dimension go to the server. plan() is a dry run and never needs confirmation, which is
 * why the UI is built to run it first: it returns embedded_rows and needs_force, so the
 * destructive step can be shown with its real cost instead of a generic warning. */
export function useEmbeddingSchema() {
  const http = useHttp();
  const [schema, setSchema] = useState<EmbeddingSchema | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);

  const refresh = useCallback(async () => {
    try {
      const res = await http.get<EmbeddingSchema>("/v1/embedding/schema");
      setSchema(res);
      setError("");
    } catch (err) {
      setSchema(null);
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setLoading(false);
    }
  }, [http]);

  useEffect(() => { refresh(); }, [refresh]);

  const plan = useCallback(
    async (to: number): Promise<RedimensionReport> =>
      http.post<RedimensionReport>("/v1/embedding/dimensions", { to, dry_run: true }),
    [http],
  );

  const apply = useCallback(
    async (to: number, force: boolean): Promise<RedimensionReport> =>
      http.post<RedimensionReport>("/v1/embedding/dimensions", { to, confirm: String(to), force }),
    [http],
  );

  return { schema, error, loading, refresh, plan, apply };
}
