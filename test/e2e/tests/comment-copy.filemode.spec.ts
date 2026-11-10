import { test, expect, type APIRequestContext, type Page } from '@playwright/test';
import { clearAllComments, getMdPath, loadPage, mdSection } from './helpers';

const AUTHOR = 'Reviewer';

async function addLineComment(request: APIRequestContext, path: string, startLine: number, endLine: number, body: string): Promise<{ id: string }> {
  const resp = await request.post(`/api/file/comments?path=${encodeURIComponent(path)}`, {
    data: { start_line: startLine, end_line: endLine, body, author: AUTHOR },
  });
  expect(resp.ok()).toBeTruthy();
  return resp.json();
}

async function readClipboard(page: Page): Promise<string> {
  return page.evaluate(() => navigator.clipboard.readText());
}

// ============================================================
// Plain-text comment copying — File Mode
// ============================================================
test.describe('Comment copy — File Mode', () => {
  test.beforeEach(async ({ request, context }) => {
    await clearAllComments(request);
    await context.grantPermissions(['clipboard-read', 'clipboard-write']);
  });

  test('card copy button copies the comment as plain text', async ({ page, request }) => {
    const mdPath = await getMdPath(request);
    await addLineComment(request, mdPath, 3, 5, 'Tighten this overview.');
    await loadPage(page);

    const copyBtn = mdSection(page).locator('.comment-card .comment-actions button[title="Copy comment"]');
    await copyBtn.click();

    await expect(copyBtn).toHaveAttribute('aria-label', 'Copied!');
    expect(await readClipboard(page)).toBe(
      `File: ${mdPath}\nLine(s): 3-5\nAuthor: ${AUTHOR}\n\nTighten this overview.`,
    );
    await expect(copyBtn).toHaveAttribute('aria-label', 'Copy comment', { timeout: 5_000 });
  });

  test('finish dialog copies every unresolved comment as one block', async ({ page, request }) => {
    const mdPath = await getMdPath(request);
    await addLineComment(request, mdPath, 3, 3, 'Line feedback.');
    const resolved = await addLineComment(request, mdPath, 7, 7, 'Already handled.');
    const resolveResp = await request.put(`/api/comment/${resolved.id}/resolve?path=${encodeURIComponent(mdPath)}`, {
      data: { resolved: true },
    });
    expect(resolveResp.ok()).toBeTruthy();
    const reviewResp = await request.post('/api/comments', { data: { body: 'General feedback.', author: AUTHOR } });
    expect(reviewResp.ok()).toBeTruthy();
    await loadPage(page);

    await page.locator('#finishBtn').click();
    await expect(page.locator('#waitingOverlay')).toHaveClass(/active/);

    const row = page.locator('#commentsCopyRow');
    await expect(row).toBeVisible();
    await expect(row.locator('.prompt-preview')).toHaveText('2 unresolved comments as plain text');

    const copyBtn = page.locator('#commentsClipboard');
    await copyBtn.click();

    await expect(copyBtn.locator('.copy-label')).toHaveText('Copied');
    expect(await readClipboard(page)).toBe(
      `Author: ${AUTHOR}\n\nGeneral feedback.` +
      '\n\n\n' +
      `File: ${mdPath}\nLine(s): 3\nAuthor: ${AUTHOR}\n\nLine feedback.`,
    );
  });
});
