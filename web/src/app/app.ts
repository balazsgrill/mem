import { Component, OnInit, signal } from '@angular/core';
import { CommonModule } from '@angular/common';
import { FormsModule } from '@angular/forms';
import { ApiService } from './services/api.service';
import { BucketSummary, MemoryDetail, AgentToken } from './models';

interface Toast {
  id: number;
  message: string;
  type: 'success' | 'error' | 'info';
}

@Component({
  selector: 'app-root',
  standalone: true,
  imports: [CommonModule, FormsModule],
  templateUrl: './app.html',
  styleUrls: ['./app.css']
})
export class App implements OnInit {
  activeTab = signal<'buckets' | 'tokens' | 'sandbox'>('buckets');
  
  // Auth state
  masterTokenInput = '';
  authError = signal<string>('');
  isAuthenticating = signal<boolean>(false);

  // Buckets & Memories
  buckets = signal<BucketSummary[]>([]);
  selectedBucket = signal<string>(''); // empty string = all
  entries = signal<MemoryDetail[]>([]);
  totalEntries = signal<number>(0);
  isLoadingEntries = signal<boolean>(false);

  // Filters
  searchQuery = '';
  filterTier = '';
  includeSuperseded = false;
  bucketSearch = '';

  // Memory Modal
  isMemoryModalOpen = signal<boolean>(false);
  isEditingMemory = signal<boolean>(false);
  memoryForm = {
    id: 0,
    bucket: '',
    tier: 'semantic',
    subject: '',
    content: '',
    metadataStr: '{}'
  };

  // Agent Tokens
  tokens = signal<AgentToken[]>([]);
  isLoadingTokens = signal<boolean>(false);
  isTokenModalOpen = signal<boolean>(false);
  isEditingToken = signal<boolean>(false);
  tokenForm = {
    token: '',
    agent_id: '',
    description: '',
    read_buckets: [] as string[],
    write_buckets: [] as string[],
    newReadBucket: '',
    newWriteBucket: ''
  };

  // Empty Bucket Modal
  isEmptyBucketModalOpen = signal<boolean>(false);
  bucketToEmpty = signal<string>('');
  permanentEmpty = false;

  // Sandbox Tab
  sandboxQuery = 'What DNS server is configured in homelab?';
  sandboxBuckets: string[] = [];
  sandboxTopK = 5;
  sandboxResult = signal<any>(null);
  isSandboxing = signal<boolean>(false);

  // Toasts
  toasts = signal<Toast[]>([]);
  private toastCounter = 0;

  constructor(public api: ApiService) {}

  ngOnInit() {
    if (this.api.isAuthenticated()) {
      this.loadAllData();
    }
  }

  // --- Auth ---
  submitMasterToken() {
    if (!this.masterTokenInput.trim()) return;
    this.isAuthenticating.set(true);
    this.authError.set('');

    this.api.setToken(this.masterTokenInput.trim());
    this.api.testAuth().subscribe({
      next: () => {
        this.isAuthenticating.set(false);
        this.loadAllData();
        this.showToast('Successfully authenticated as Master', 'success');
      },
      error: (err) => {
        this.isAuthenticating.set(false);
        this.api.setToken('');
        this.authError.set('Invalid Master Token. Access denied.');
      }
    });
  }

  logout() {
    this.api.logout();
    this.masterTokenInput = '';
    this.showToast('Logged out', 'info');
  }

  loadAllData() {
    this.loadBuckets();
    this.loadEntries();
    this.loadTokens();
  }

  // --- Buckets & Entries ---
  loadBuckets() {
    this.api.getBuckets().subscribe({
      next: (b) => {
        this.buckets.set(b);
        if (this.sandboxBuckets.length === 0 && b.length > 0) {
          this.sandboxBuckets = [b[0].bucket];
        }
      },
      error: (err) => this.showToast('Failed loading buckets: ' + err.message, 'error')
    });
  }

  selectBucket(bucket: string) {
    this.selectedBucket.set(bucket);
    this.loadEntries();
  }

  loadEntries() {
    this.isLoadingEntries.set(true);
    this.api.getEntries(
      this.selectedBucket(),
      this.filterTier,
      this.searchQuery,
      this.includeSuperseded,
      100,
      0
    ).subscribe({
      next: (res) => {
        this.entries.set(res.entries || []);
        this.totalEntries.set(res.total || 0);
        this.isLoadingEntries.set(false);
      },
      error: (err) => {
        this.isLoadingEntries.set(false);
        this.showToast('Failed loading entries: ' + err.message, 'error');
      }
    });
  }

  filteredBuckets(): BucketSummary[] {
    const q = this.bucketSearch.toLowerCase().trim();
    if (!q) return this.buckets();
    return this.buckets().filter(b => b.bucket.toLowerCase().includes(q));
  }

  openAddMemoryModal(defaultBucket?: string) {
    this.isEditingMemory.set(false);
    this.memoryForm = {
      id: 0,
      bucket: defaultBucket || this.selectedBucket() || (this.buckets()[0]?.bucket || 'general'),
      tier: 'semantic',
      subject: '',
      content: '',
      metadataStr: '{}'
    };
    this.isMemoryModalOpen.set(true);
  }

  openEditMemoryModal(entry: MemoryDetail) {
    this.isEditingMemory.set(true);
    this.memoryForm = {
      id: entry.id,
      bucket: entry.bucket,
      tier: entry.tier,
      subject: entry.subject,
      content: entry.content,
      metadataStr: JSON.stringify(entry.metadata || {}, null, 2)
    };
    this.isMemoryModalOpen.set(true);
  }

  saveMemory() {
    if (!this.memoryForm.bucket.trim() || !this.memoryForm.subject.trim() || !this.memoryForm.content.trim()) {
      this.showToast('Please fill in Bucket, Subject, and Content', 'error');
      return;
    }

    let meta: Record<string, any> = {};
    try {
      if (this.memoryForm.metadataStr.trim()) {
        meta = JSON.parse(this.memoryForm.metadataStr);
      }
    } catch (e: any) {
      this.showToast('Metadata must be valid JSON: ' + e.message, 'error');
      return;
    }

    if (this.isEditingMemory()) {
      this.api.updateEntry(this.memoryForm.id, {
        bucket: this.memoryForm.bucket.trim(),
        tier: this.memoryForm.tier,
        subject: this.memoryForm.subject.trim(),
        content: this.memoryForm.content.trim(),
        metadata: meta
      }).subscribe({
        next: () => {
          this.isMemoryModalOpen.set(false);
          this.showToast('Memory updated successfully', 'success');
          this.loadEntries();
          this.loadBuckets();
        },
        error: (err) => this.showToast('Failed updating memory: ' + err.message, 'error')
      });
    } else {
      this.api.addEntry({
        bucket: this.memoryForm.bucket.trim(),
        tier: this.memoryForm.tier,
        subject: this.memoryForm.subject.trim(),
        content: this.memoryForm.content.trim(),
        metadata: meta
      }).subscribe({
        next: () => {
          this.isMemoryModalOpen.set(false);
          this.showToast('Memory created successfully', 'success');
          this.loadEntries();
          this.loadBuckets();
        },
        error: (err) => this.showToast('Failed creating memory: ' + err.message, 'error')
      });
    }
  }

  deleteMemory(id: number, permanent: boolean = false) {
    const action = permanent ? 'permanently delete' : 'invalidate';
    if (!confirm(`Are you sure you want to ${action} memory ID #${id}?`)) {
      return;
    }

    this.api.deleteEntry(id, permanent).subscribe({
      next: () => {
        this.showToast(`Memory #${id} ${permanent ? 'deleted' : 'invalidated'}`, 'success');
        this.loadEntries();
        this.loadBuckets();
      },
      error: (err) => this.showToast('Failed deleting memory: ' + err.message, 'error')
    });
  }

  openEmptyBucketModal(bucket: string) {
    this.bucketToEmpty.set(bucket);
    this.permanentEmpty = false;
    this.isEmptyBucketModalOpen.set(true);
  }

  confirmEmptyBucket() {
    const b = this.bucketToEmpty();
    if (!b) return;

    this.api.emptyBucket(b, this.permanentEmpty).subscribe({
      next: (res) => {
        this.isEmptyBucketModalOpen.set(false);
        this.showToast(`Bucket "${b}" emptied (${res.affected} records affected)`, 'success');
        this.loadBuckets();
        this.loadEntries();
      },
      error: (err) => this.showToast('Failed emptying bucket: ' + err.message, 'error')
    });
  }

  // --- Agent Tokens ---
  loadTokens() {
    this.isLoadingTokens.set(true);
    this.api.getTokens('', true).subscribe({
      next: (toks) => {
        this.tokens.set(toks || []);
        this.isLoadingTokens.set(false);
      },
      error: (err) => {
        this.isLoadingTokens.set(false);
        this.showToast('Failed loading tokens: ' + err.message, 'error');
      }
    });
  }

  openCreateTokenModal() {
    this.isEditingToken.set(false);
    this.tokenForm = {
      token: '',
      agent_id: '',
      description: '',
      read_buckets: [],
      write_buckets: [],
      newReadBucket: '',
      newWriteBucket: ''
    };
    this.isTokenModalOpen.set(true);
  }

  openEditTokenModal(tok: AgentToken) {
    this.isEditingToken.set(true);
    this.tokenForm = {
      token: tok.token,
      agent_id: tok.agent_id,
      description: tok.description || '',
      read_buckets: [...(tok.read_buckets || [])],
      write_buckets: [...(tok.write_buckets || [])],
      newReadBucket: '',
      newWriteBucket: ''
    };
    this.isTokenModalOpen.set(true);
  }

  addBucketToToken(type: 'read' | 'write', bucketName?: string) {
    const name = (bucketName || (type === 'read' ? this.tokenForm.newReadBucket : this.tokenForm.newWriteBucket)).trim();
    if (!name) return;

    if (type === 'read') {
      if (!this.tokenForm.read_buckets.includes(name)) {
        this.tokenForm.read_buckets.push(name);
      }
      this.tokenForm.newReadBucket = '';
    } else {
      if (!this.tokenForm.write_buckets.includes(name)) {
        this.tokenForm.write_buckets.push(name);
      }
      this.tokenForm.newWriteBucket = '';
    }
  }

  removeBucketFromToken(type: 'read' | 'write', index: number) {
    if (type === 'read') {
      this.tokenForm.read_buckets.splice(index, 1);
    } else {
      this.tokenForm.write_buckets.splice(index, 1);
    }
  }

  saveToken() {
    if (!this.tokenForm.agent_id.trim()) {
      this.showToast('Agent ID is required', 'error');
      return;
    }

    if (this.isEditingToken()) {
      this.api.updateToken(this.tokenForm.token, {
        agent_id: this.tokenForm.agent_id.trim(),
        description: this.tokenForm.description.trim(),
        read_buckets: this.tokenForm.read_buckets,
        write_buckets: this.tokenForm.write_buckets
      }).subscribe({
        next: () => {
          this.isTokenModalOpen.set(false);
          this.showToast('Agent token updated successfully', 'success');
          this.loadTokens();
        },
        error: (err) => this.showToast('Failed updating token: ' + err.message, 'error')
      });
    } else {
      this.api.createToken({
        token: this.tokenForm.token.trim() || undefined,
        agent_id: this.tokenForm.agent_id.trim(),
        description: this.tokenForm.description.trim(),
        read_buckets: this.tokenForm.read_buckets,
        write_buckets: this.tokenForm.write_buckets
      }).subscribe({
        next: () => {
          this.isTokenModalOpen.set(false);
          this.showToast('Agent token created successfully', 'success');
          this.loadTokens();
        },
        error: (err) => this.showToast('Failed creating token: ' + err.message, 'error')
      });
    }
  }

  toggleTokenRevocation(tok: AgentToken) {
    const newRevoked = !tok.revoked;
    this.api.updateToken(tok.token, { revoked: newRevoked }).subscribe({
      next: () => {
        this.showToast(`Token for ${tok.agent_id} ${newRevoked ? 'revoked' : 'reactivated'}`, 'success');
        this.loadTokens();
      },
      error: (err) => this.showToast('Failed updating token status: ' + err.message, 'error')
    });
  }

  deleteToken(tokenStr: string, permanent: boolean = true) {
    if (!confirm('Are you sure you want to delete this agent token permanently?')) {
      return;
    }
    this.api.deleteToken(tokenStr, permanent).subscribe({
      next: () => {
        this.showToast('Token deleted', 'success');
        this.loadTokens();
      },
      error: (err) => this.showToast('Failed deleting token: ' + err.message, 'error')
    });
  }

  copyToClipboard(text: string, label: string = 'Token') {
    navigator.clipboard.writeText(text).then(() => {
      this.showToast(`${label} copied to clipboard!`, 'info');
    });
  }

  // --- Prompt Sandbox ---
  toggleSandboxBucket(b: string) {
    const idx = this.sandboxBuckets.indexOf(b);
    if (idx >= 0) {
      this.sandboxBuckets.splice(idx, 1);
    } else {
      this.sandboxBuckets.push(b);
    }
  }

  isSandboxBucketSelected(b: string): boolean {
    return this.sandboxBuckets.includes(b);
  }

  runSandbox() {
    if (!this.sandboxQuery.trim()) return;
    this.isSandboxing.set(true);
    this.sandboxResult.set(null);

    this.api.hydratePrompt(this.sandboxQuery, this.sandboxBuckets, this.sandboxTopK).subscribe({
      next: (res) => {
        this.sandboxResult.set(res);
        this.isSandboxing.set(false);
      },
      error: (err) => {
        this.isSandboxing.set(false);
        this.showToast('Prompt hydration failed: ' + err.message, 'error');
      }
    });
  }

  // --- Toasts ---
  showToast(message: string, type: 'success' | 'error' | 'info' = 'info') {
    const id = ++this.toastCounter;
    this.toasts.update(t => [...t, { id, message, type }]);
    setTimeout(() => {
      this.toasts.update(t => t.filter(x => x.id !== id));
    }, 4000);
  }

  removeToast(id: number) {
    this.toasts.update(t => t.filter(x => x.id !== id));
  }
}
