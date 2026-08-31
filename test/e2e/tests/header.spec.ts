import { test, expect, type Page } from '@playwright/test';
import { clearAllComments, loadPage, setPageVisibility } from './helpers';

type PRCheck = { name: string; state: string; url?: string };
type PRReview = { author: string; state: string };
type PRStatusFixture = {
  head_sha: string;
  checks: PRCheck[];
  latest_reviews: PRReview[];
  review_decision: string;
  mergeable: string;
  merge_state_status: string;
  unresolved_review_thread_count: number;
  base_requires_merge_queue: boolean;
  queued: boolean;
  allowed_merge_methods: string[];
  default_merge_method?: string;
  ready: boolean;
  blocking_reasons: string[];
};

async function mockPRConfig(page: Page): Promise<void> {
  await page.route('**/api/config', async (route) => {
    const response = await route.fetch();
    const config = await response.json();
    await route.fulfill({
      response,
      json: {
        ...config,
        pr_url: 'https://github.com/example/project/pull/42',
        pr_number: 42,
        pr_title: 'Ship PR controls',
        pr_state: 'OPEN',
        pr_is_draft: false,
      },
    });
  });
}

function readyPRStatus(overrides: Partial<PRStatusFixture> = {}): PRStatusFixture {
  return {
    head_sha: 'abc123def456',
    checks: [{ name: 'test', state: 'SUCCESS', url: 'https://github.com/example/project/actions/1' }],
    latest_reviews: [{ author: 'reviewer', state: 'APPROVED' }],
    review_decision: 'APPROVED',
    mergeable: 'MERGEABLE',
    merge_state_status: 'CLEAN',
    unresolved_review_thread_count: 0,
    base_requires_merge_queue: false,
    queued: false,
    allowed_merge_methods: ['squash'],
    ready: true,
    blocking_reasons: [],
    ...overrides,
  };
}

async function mockPRStatus(page: Page, status: PRStatusFixture): Promise<void> {
  await page.route('**/api/change/status', async (route) => {
    await route.fulfill({ status: 200, json: status });
  });
}

async function openPRPanel(page: Page): Promise<void> {
  await page.locator('#prToggle').click();
  await expect(page.locator('#prPanel')).toBeVisible();
}

test.describe('Review header', () => {
  test.beforeEach(async ({ request }) => {
    await clearAllComments(request);
  });

  test('shows a direct PR link without an update button', async ({ page }) => {
    await page.route('**/api/config', async (route) => {
      const response = await route.fetch();
      const config = await response.json();
      await route.fulfill({
        response,
        json: {
          ...config,
          pr_url: 'https://github.com/example/project/pull/42',
          pr_number: 42,
        },
      });
    });

    await loadPage(page);

    const prLink = page.locator('#headerPrLink');
    await expect(prLink).toBeVisible();
    await expect(prLink).toHaveText('PR #42');
    await expect(prLink).toHaveAttribute('href', 'https://github.com/example/project/pull/42');
    await expect(page.locator('#updateBtn')).toHaveCount(0);
  });

  test('fetch control refreshes the selected comparison branch', async ({ page }) => {
    let requestedRef = '';
    await page.route('**/api/base-branch/fetch', async (route) => {
      const body = route.request().postDataJSON() as { ref: string };
      requestedRef = body.ref;
      await route.fulfill({ status: 502, body: 'test fetch failure' });
    });
    await loadPage(page);

    const fetchButton = page.locator('#compareRefreshBtn');
    await expect(fetchButton).toBeVisible();
    await fetchButton.click();

    await expect.poll(() => requestedRef).not.toBe('');
    await expect(page.locator('.mini-toast')).toContainText('test fetch failure');
  });
});

test.describe('Pull request panel', () => {
  test.beforeEach(async ({ request }) => {
    await clearAllComments(request);
  });

  test('renders ready status, checks, reviews, and unresolved threads', async ({ page }) => {
    await mockPRConfig(page);
    await mockPRStatus(page, readyPRStatus());
    await loadPage(page);
    await openPRPanel(page);

    const panel = page.locator('#prPanel');
    await expect(panel.locator('.pr-panel-readiness')).toContainText('Ready to merge');
    await expect(panel.locator('.pr-panel-status-summary')).toContainText('0 unresolved threads');
    await expect(panel.locator('.pr-panel-status-group').filter({ hasText: 'Checks' })).toContainText('test');
    await expect(panel.locator('.pr-panel-status-group').filter({ hasText: 'Reviews' })).toContainText('reviewer');
    await expect(panel.locator('.pr-panel-status-marker-success')).toHaveCount(2);
  });

  test('renders blocking reasons and each check state', async ({ page }) => {
    await mockPRConfig(page);
    await mockPRStatus(page, readyPRStatus({
      ready: false,
      review_decision: 'REVIEW_REQUIRED',
      unresolved_review_thread_count: 2,
      blocking_reasons: ['Waiting for required checks', 'Resolve review threads'],
      checks: [
        { name: 'lint', state: 'SUCCESS' },
        { name: 'integration', state: 'IN_PROGRESS' },
        { name: 'deploy', state: 'FAILURE' },
      ],
      latest_reviews: [{ author: 'security-reviewer', state: 'CHANGES_REQUESTED' }],
    }));
    await loadPage(page);
    await openPRPanel(page);

    const panel = page.locator('#prPanel');
    await expect(panel.locator('.pr-panel-readiness')).toContainText('Needs attention');
    await expect(panel.locator('.pr-panel-blocking-reasons')).toContainText('Waiting for required checks');
    await expect(panel.locator('.pr-panel-blocking-reasons')).toContainText('Resolve review threads');
    await expect(panel.locator('.pr-panel-status-marker-success')).toHaveCount(1);
    await expect(panel.locator('.pr-panel-status-marker-pending')).toHaveCount(1);
    await expect(panel.locator('.pr-panel-status-marker-failure')).toHaveCount(2);
    await expect(page.getByRole('button', { name: 'Merge', exact: true })).toBeDisabled();
  });

  test('syncs comments explicitly and reports the result', async ({ page }) => {
    let syncCalls = 0;
    await mockPRConfig(page);
    await mockPRStatus(page, readyPRStatus());
    await page.route('**/api/change/comments/sync', async (route) => {
      syncCalls++;
      await route.fulfill({ status: 200, json: { added: 2 } });
    });
    await loadPage(page);
    await openPRPanel(page);

    await page.getByRole('button', { name: 'Sync now' }).click();
    await expect.poll(() => syncCalls).toBe(1);
    await expect(page.locator('.mini-toast')).toContainText('2 comments synced');
  });

  test('adds to the merge queue without sending a method', async ({ page }) => {
    let mergeRequest = '';
    await mockPRConfig(page);
    await mockPRStatus(page, readyPRStatus({
      base_requires_merge_queue: true,
      allowed_merge_methods: ['merge', 'squash'],
    }));
    await page.route('**/api/change/merge', async (route) => {
      mergeRequest = JSON.stringify(route.request().postDataJSON());
      await route.fulfill({ status: 200, json: { queued: true, merged: false, message: 'Queued' } });
    });
    page.on('dialog', async (dialog) => dialog.accept());
    await loadPage(page);
    await openPRPanel(page);

    await expect(page.locator('.pr-panel-merge-method')).toHaveCount(0);
    await page.getByRole('button', { name: 'Add to merge queue' }).click();
    await expect.poll(() => mergeRequest).toBe(JSON.stringify({ head_sha: 'abc123def456' }));
  });

  test('preselects the viewer default and sends the selected direct merge method', async ({ page }) => {
    let mergeRequest = '';
    await mockPRConfig(page);
    await mockPRStatus(page, readyPRStatus({
      allowed_merge_methods: ['merge', 'squash', 'rebase'],
      default_merge_method: 'REBASE',
    }));
    await page.route('**/api/change/merge', async (route) => {
      mergeRequest = JSON.stringify(route.request().postDataJSON());
      await route.fulfill({ status: 200, json: { queued: false, merged: true, message: 'Merged' } });
    });
    page.on('dialog', async (dialog) => dialog.accept());
    await loadPage(page);
    await openPRPanel(page);

    const method = page.locator('.pr-panel-merge-method');
    await expect(method).toHaveValue('rebase');
    await method.selectOption('merge');
    await page.getByRole('button', { name: 'Merge', exact: true }).click();
    await expect.poll(() => mergeRequest).toBe(JSON.stringify({ head_sha: 'abc123def456', method: 'merge' }));
  });

  test('shows merge failures returned by the backend', async ({ page }) => {
    await mockPRConfig(page);
    await mockPRStatus(page, readyPRStatus());
    await page.route('**/api/change/merge', async (route) => {
      await route.fulfill({ status: 409, json: { error: 'Head SHA is stale; refresh and try again' } });
    });
    page.on('dialog', async (dialog) => dialog.accept());
    await loadPage(page);
    await openPRPanel(page);

    await page.getByRole('button', { name: 'Merge', exact: true }).click();
    await expect(page.locator('.mini-toast')).toContainText('Head SHA is stale; refresh and try again');
  });

  test('refreshes open-panel status every minute', async ({ page }) => {
    let statusCalls = 0;
    await page.clock.install();
    await mockPRConfig(page);
    await page.route('**/api/change/status', async (route) => {
      statusCalls++;
      await route.fulfill({ status: 200, json: readyPRStatus() });
    });
    await loadPage(page);
    await openPRPanel(page);
    await expect.poll(() => statusCalls).toBe(1);

    await page.clock.fastForward(60_000);
    await expect.poll(() => statusCalls).toBe(2);
  });

  test('catches up comment sync after returning from five hidden minutes', async ({ page }) => {
    let syncCalls = 0;
    await page.clock.install();
    await mockPRConfig(page);
    await page.route('**/api/change/comments/sync', async (route) => {
      syncCalls++;
      await route.fulfill({ status: 200, json: { added: 0 } });
    });
    await loadPage(page);
    await setPageVisibility(page, false);

    await page.clock.fastForward(5 * 60_000);
    expect(syncCalls).toBe(0);
    await setPageVisibility(page, true);
    await expect.poll(() => syncCalls).toBe(1);
  });
});
