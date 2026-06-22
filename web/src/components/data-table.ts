import { html, type TemplateResult } from "lit";
import { customElement, property } from "lit/decorators.js";
import {
  createTable,
  getCoreRowModel,
  getSortedRowModel,
  getPaginationRowModel,
  functionalUpdate,
  type ColumnDef,
  type Table,
  type TableState,
} from "@tanstack/table-core";
import { AppElement } from "./base.js";
import { t } from "../i18n.js";
import { fieldHelp } from "../icons.js";

// A column for <data-table>. accessor feeds sorting and the default cell text;
// cell optionally renders custom markup (a link, a badge) from the row.
export interface DataColumn {
  id: string;
  header: string;
  // optional info-hint text shown next to the header (localized by the caller)
  headerHint?: string;
  accessor?: (row: unknown) => string | number | null;
  sortable?: boolean;
  cell?: (row: unknown) => TemplateResult | string;
  class?: string;
}

// Reusable sortable + paginated table (RFC-013 decision D13). daisyUI `.table`
// styles the markup; TanStack Table (headless) owns the sort/paginate behavior.
//
// table-core is fully controlled, so we keep one persistent table instance and
// own the full TableState ourselves: onStateChange folds the updater into our
// state, writes it back, and requests a Lit re-render.
@customElement("data-table")
export class DataTable extends AppElement {
  @property({ attribute: false }) columns: DataColumn[] = [];
  @property({ attribute: false }) data: unknown[] = [];
  @property({ type: Number }) pageSize = 10;
  // When set, each row gets a toggle in a leading cell and this renders the detail
  // shown below it when open. A row whose renderDetail returns null has no toggle.
  @property({ attribute: false }) renderDetail?: (row: unknown) => TemplateResult | null;

  private table?: Table<unknown>;
  private tableState!: TableState;
  private expanded = new Set<string>();

  private toggleRow(id: string): void {
    if (this.expanded.has(id)) this.expanded.delete(id);
    else this.expanded.add(id);
    this.requestUpdate();
  }

  private tanstackColumns(): ColumnDef<unknown>[] {
    return this.columns.map((c) => ({
      id: c.id,
      header: c.header,
      accessorFn: c.accessor ?? (() => ""),
      enableSorting: c.sortable ?? false,
    }));
  }

  private ensureTable(): Table<unknown> {
    if (this.table) {
      // keep data/columns in sync with the latest props each render
      this.table.setOptions((prev) => ({
        ...prev,
        data: this.data,
        columns: this.tanstackColumns(),
      }));
      return this.table;
    }

    const table = createTable<unknown>({
      data: this.data,
      columns: this.tanstackColumns(),
      state: {},
      onStateChange: () => {},
      renderFallbackValue: null,
      getCoreRowModel: getCoreRowModel(),
      getSortedRowModel: getSortedRowModel(),
      getPaginationRowModel: getPaginationRowModel(),
    });

    // seed our state from the table's full defaults, with our page size
    this.tableState = {
      ...table.initialState,
      pagination: { ...table.initialState.pagination, pageSize: this.pageSize },
    };

    table.setOptions((prev) => ({
      ...prev,
      state: this.tableState,
      onStateChange: (updater) => {
        this.tableState = functionalUpdate(updater, this.tableState);
        this.table?.setOptions((p) => ({ ...p, state: this.tableState }));
        this.requestUpdate();
      },
    }));

    this.table = table;
    return table;
  }

  override render() {
    const table = this.ensureTable();
    const header = table.getHeaderGroups()[0];
    const rows = table.getRowModel().rows;
    const pageCount = table.getPageCount();
    const expandable = !!this.renderDetail;
    const colCount = this.columns.length + (expandable ? 1 : 0);

    return html`
      <div class="overflow-x-auto rounded-box border border-base-200">
        <table class="table table-zebra">
          <thead>
            <tr>
              ${expandable ? html`<th class="w-8"></th>` : ""}
              ${header.headers.map((h) => {
                const col = h.column;
                const sortable = col.getCanSort();
                const sorted = col.getIsSorted();
                const hint = this.columns.find((c) => c.id === col.id)?.headerHint;
                return html`<th
                  class="text-xs uppercase tracking-wide ${sortable
                    ? "cursor-pointer select-none"
                    : ""}"
                >
                  <span class="inline-flex items-center gap-1">
                    <span
                      class="inline-flex items-center gap-1 ${sortable
                        ? "hover:text-base-content"
                        : ""}"
                      @click=${sortable ? col.getToggleSortingHandler() : null}
                      ?data-sortable=${sortable}
                    >
                      ${String(col.columnDef.header)}
                      <span class="text-primary"
                        >${sorted === "asc" ? "▲" : sorted === "desc" ? "▼" : ""}</span
                      >
                    </span>
                    ${hint ? fieldHelp(hint) : ""}
                  </span>
                </th>`;
              })}
            </tr>
          </thead>
          <tbody>
            ${rows.map((row) => {
              const detail = expandable ? this.renderDetail!(row.original) : null;
              const open = this.expanded.has(row.id);
              return html`<tr class="hover">
                  ${expandable
                    ? html`<td class="w-8">
                        ${detail
                          ? html`<button
                              class="btn btn-ghost btn-xs"
                              aria-expanded=${open ? "true" : "false"}
                              @click=${() => this.toggleRow(row.id)}
                            >
                              ${open ? "▾" : "▸"}
                            </button>`
                          : ""}
                      </td>`
                    : ""}
                  ${this.columns.map(
                    (c) => html`<td class=${c.class ?? ""}>
                      ${c.cell
                        ? c.cell(row.original)
                        : String(row.getValue(c.id) ?? "")}
                    </td>`,
                  )}
                </tr>
                ${detail && open
                  ? html`<tr class="bg-base-200/40">
                      <td colspan=${colCount} class="p-0">${detail}</td>
                    </tr>`
                  : ""}`;
            })}
          </tbody>
        </table>
      </div>
      ${pageCount > 1
        ? html`<div class="flex items-center justify-end gap-2 mt-2">
            <button
              class="btn btn-sm btn-ghost"
              ?disabled=${!table.getCanPreviousPage()}
              @click=${() => table.previousPage()}
            >
              ${t("table.previous")}
            </button>
            <span class="text-sm text-base-content/60">
              ${table.getState().pagination.pageIndex + 1} / ${pageCount}
            </span>
            <button
              class="btn btn-sm btn-ghost"
              ?disabled=${!table.getCanNextPage()}
              @click=${() => table.nextPage()}
            >
              ${t("table.next")}
            </button>
          </div>`
        : ""}
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    "data-table": DataTable;
  }
}
