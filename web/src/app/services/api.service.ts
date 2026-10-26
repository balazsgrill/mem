import { Injectable, signal } from '@angular/core';
import { HttpClient, HttpHeaders, HttpParams } from '@angular/common/http';
import { Observable, tap } from 'rxjs';
import { BucketSummary, MemoryDetail, AgentToken, AgentTokenUpdate } from '../models';

@Injectable({
  providedIn: 'root'
})
export class ApiService {
  token = signal<string>('');
  isAuthenticated = signal<boolean>(false);

  constructor(private http: HttpClient) {
    this.detectInitialToken();
  }

  private detectInitialToken() {
    // 1. Check URL query params (?token=...)
    const urlParams = new URLSearchParams(window.location.search);
    const queryToken = urlParams.get('token');
    if (queryToken) {
      this.setToken(queryToken);
      // Clean query string from browser URL
      const cleanUrl = window.location.pathname;
      window.history.replaceState({}, document.title, cleanUrl);
      return;
    }

    // 2. Check path /ui/<token>
    const pathParts = window.location.pathname.split('/').filter(Boolean);
    if (pathParts.length >= 2 && pathParts[0] === 'ui' && !['buckets', 'tokens', 'hydrate'].includes(pathParts[1])) {
      const pathToken = pathParts[1];
      if (pathToken && pathToken.length > 5) {
        this.setToken(pathToken);
        window.history.replaceState({}, document.title, '/ui/');
        return;
      }
    }

    // 3. Check Cookie
    const match = document.cookie.match(new RegExp('(^| )mem_master_token=([^;]+)'));
    if (match && match[2]) {
      this.token.set(match[2]);
      this.isAuthenticated.set(true);
      return;
    }

    // 4. Check LocalStorage
    const stored = localStorage.getItem('mem_master_token');
    if (stored) {
      this.token.set(stored);
      this.isAuthenticated.set(true);
    }
  }

  setToken(tok: string) {
    const trimmed = tok.trim();
    this.token.set(trimmed);
    this.isAuthenticated.set(!!trimmed);
    if (trimmed) {
      localStorage.setItem('mem_master_token', trimmed);
      document.cookie = `mem_master_token=${trimmed}; path=/; max-age=2592000; SameSite=Lax`;
    } else {
      localStorage.removeItem('mem_master_token');
      document.cookie = 'mem_master_token=; path=/; max-age=0';
    }
  }

  logout() {
    this.setToken('');
  }

  private getHeaders(): HttpHeaders {
    let headers = new HttpHeaders();
    if (this.token()) {
      headers = headers.set('Authorization', `Bearer ${this.token()}`);
    }
    return headers;
  }

  // Verification / Health
  testAuth(): Observable<any> {
    return this.http.get<any>('/api/tokens', { headers: this.getHeaders() });
  }

  // --- Buckets ---
  getBuckets(): Observable<BucketSummary[]> {
    return this.http.get<BucketSummary[]>('/api/buckets', { headers: this.getHeaders() });
  }

  emptyBucket(bucket: string, permanent: boolean = false): Observable<{ status: string; bucket: string; affected: number }> {
    const params = new HttpParams().set('permanent', String(permanent));
    return this.http.delete<any>(`/api/buckets/${encodeURIComponent(bucket)}`, {
      headers: this.getHeaders(),
      params
    });
  }

  // --- Entries ---
  getEntries(
    bucket?: string,
    tier?: string,
    q?: string,
    includeSuperseded: boolean = false,
    limit: number = 50,
    offset: number = 0
  ): Observable<{ entries: MemoryDetail[]; total: number }> {
    let params = new HttpParams()
      .set('include_superseded', String(includeSuperseded))
      .set('limit', String(limit))
      .set('offset', String(offset));

    if (bucket) params = params.set('bucket', bucket);
    if (tier) params = params.set('tier', tier);
    if (q) params = params.set('q', q);

    return this.http.get<{ entries: MemoryDetail[]; total: number }>('/api/entries', {
      headers: this.getHeaders(),
      params
    });
  }

  getEntry(id: number): Observable<MemoryDetail> {
    return this.http.get<MemoryDetail>(`/api/entries/${id}`, { headers: this.getHeaders() });
  }

  addEntry(data: {
    bucket: string;
    tier: string;
    subject: string;
    content: string;
    metadata?: Record<string, any>;
  }): Observable<MemoryDetail> {
    return this.http.post<MemoryDetail>('/api/entries', data, { headers: this.getHeaders() });
  }

  updateEntry(
    id: number,
    data: {
      bucket?: string;
      tier?: string;
      subject?: string;
      content?: string;
      metadata?: Record<string, any>;
    }
  ): Observable<MemoryDetail> {
    return this.http.put<MemoryDetail>(`/api/entries/${id}`, data, { headers: this.getHeaders() });
  }

  deleteEntry(id: number, permanent: boolean = false): Observable<{ status: string; id: number }> {
    const params = new HttpParams().set('permanent', String(permanent));
    return this.http.delete<{ status: string; id: number }>(`/api/entries/${id}`, {
      headers: this.getHeaders(),
      params
    });
  }

  // --- Agent Tokens ---
  getTokens(agentId?: string, includeRevoked: boolean = true): Observable<AgentToken[]> {
    let params = new HttpParams().set('include_revoked', String(includeRevoked));
    if (agentId) params = params.set('agent_id', agentId);
    return this.http.get<AgentToken[]>('/api/tokens', {
      headers: this.getHeaders(),
      params
    });
  }

  createToken(data: Partial<AgentToken>): Observable<AgentToken> {
    return this.http.post<AgentToken>('/api/tokens', data, { headers: this.getHeaders() });
  }

  updateToken(tokenStr: string, data: AgentTokenUpdate): Observable<AgentToken> {
    return this.http.put<AgentToken>(`/api/tokens/${encodeURIComponent(tokenStr)}`, data, {
      headers: this.getHeaders()
    });
  }

  deleteToken(tokenStr: string, permanent: boolean = false): Observable<{ status: string }> {
    const params = new HttpParams().set('permanent', String(permanent));
    return this.http.delete<{ status: string }>(`/api/tokens/${encodeURIComponent(tokenStr)}`, {
      headers: this.getHeaders(),
      params
    });
  }

  // Prompt Hydration tester
  hydratePrompt(query: string, buckets: string[], topK: number = 5): Observable<any> {
    return this.http.post<any>('/api/prompt/hydrate', {
      query,
      buckets,
      top_k: topK
    }, { headers: this.getHeaders() });
  }
}
