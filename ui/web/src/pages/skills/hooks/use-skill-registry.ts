import { useCallback, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { useHttp } from "@/hooks/use-ws";
import { queryKeys } from "@/lib/query-keys";

export type RegistrySource = { name: string; searchable: boolean };

export type RegistrySummary = {
  slug: string;
  display_name: string;
  summary: string;
  version: string;
  score?: number;
  malware_blocked: boolean;
  suspicious: boolean;
  url?: string;
  source: string;
};

export type RegistryFileAnalysis = { path: string; bytes: number; mode: string };

export type RegistryAnalysis = {
  source: string;
  locator: string;
  url?: string;
  version?: string;
  name: string;
  description?: string;
  files: RegistryFileAnalysis[];
  total_bytes: number;
  executables?: string[];
  declared?: string[];
  missing_deps?: string[];
  needs_shell?: boolean;
  findings?: string[];
  unsafe: boolean;
  suspicious: boolean;
  checked_at: string;
};

export type RegistryInstallResult = {
  id: string;
  slug: string;
  version: number;
  dir: string;
  status: string;
  missing_deps?: string[];
  unchanged: boolean;
  analysis: RegistryAnalysis;
};

export type RegistryTarget = { source: string; slug: string; version?: string };

// Community sources are read on demand and mutated explicitly, so nothing here is cached except
// the source list: a search that silently served a stale index would show skills that have since
// been pulled from the registry.
export function useSkillRegistry(open: boolean) {
  const http = useHttp();
  const [searching, setSearching] = useState(false);
  const [inspecting, setInspecting] = useState(false);
  const [installing, setInstalling] = useState(false);

  const {
    data: sourcesData,
    isFetching: sourcesLoading,
    refetch: refetchSources,
  } = useQuery({
    queryKey: queryKeys.skills.registrySources,
    queryFn: () => http.get<{ enabled: boolean; sources: RegistrySource[] }>("/v1/skills/registry/sources"),
    enabled: open,
    staleTime: 60_000,
  });

  const search = useCallback(
    async (q: string, source?: string) => {
      setSearching(true);
      try {
        const params: Record<string, string> = { q };
        if (source) params.source = source;
        return await http.get<{ query: string; results: RegistrySummary[]; failures: Record<string, string> }>(
          "/v1/skills/registry/search",
          params,
        );
      } finally {
        setSearching(false);
      }
    },
    [http],
  );

  const inspect = useCallback(
    async (target: RegistryTarget) => {
      setInspecting(true);
      try {
        return await http.post<{ analysis: RegistryAnalysis; skill_md: string }>(
          "/v1/skills/registry/inspect",
          target,
        );
      } finally {
        setInspecting(false);
      }
    },
    [http],
  );

  const install = useCallback(
    async (target: RegistryTarget) => {
      setInstalling(true);
      try {
        return await http.post<RegistryInstallResult>("/v1/skills/registry/install", target);
      } finally {
        setInstalling(false);
      }
    },
    [http],
  );

  return {
    enabled: sourcesData?.enabled ?? false,
    sources: sourcesData?.sources ?? [],
    sourcesLoading,
    refetchSources,
    search,
    inspect,
    install,
    searching,
    inspecting,
    installing,
  };
}
