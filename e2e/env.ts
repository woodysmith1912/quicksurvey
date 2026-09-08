/**
 * Shared test environment. Kept out of playwright.config.ts so importing it
 * from a spec does not drag in the config's setup side effects.
 */

/** When set, a server is already running here and the config will not start one. */
export const EXTERNAL_URL = process.env.QS_E2E_BASE_URL ?? '';

export const PORT = Number(process.env.QS_TEST_PORT ?? 8099);
export const BASE_URL = EXTERNAL_URL || `http://127.0.0.1:${PORT}`;

/** The administrator seeded before the server starts, in both run modes. */
export const ADMIN = { user: 'admin', password: 'playwright-admin-pw' };
