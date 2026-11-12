// Moves storage keys a hermes build wrote to their icebath names before any store reads them.
import { migrateBrowserStorage } from '@icebath/shared/storage-migration'

migrateBrowserStorage(globalThis)
