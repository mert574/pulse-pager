import { expect, fixture, html } from "@open-wc/testing";
import "./data-table.js";
import type { DataTable, DataColumn } from "./data-table.js";

interface Row {
  name: string;
  n: number;
}

const COLUMNS: DataColumn[] = [
  {
    id: "name",
    header: "Name",
    accessor: (r) => (r as Row).name,
    sortable: true,
  },
  {
    id: "n",
    header: "N",
    accessor: (r) => (r as Row).n,
    sortable: true,
    cell: (r) => String((r as Row).n),
  },
];

const DATA: Row[] = [
  { name: "b", n: 2 },
  { name: "a", n: 1 },
  { name: "c", n: 3 },
];

function firstColumnText(el: DataTable): string[] {
  return Array.from(el.querySelectorAll("tbody tr")).map(
    (tr) => tr.querySelector("td")?.textContent?.trim() ?? "",
  );
}

describe("data-table", () => {
  it("renders a row per item", async () => {
    const el = await fixture<DataTable>(
      html`<data-table .columns=${COLUMNS} .data=${DATA} .pageSize=${10}></data-table>`,
    );
    await el.updateComplete;
    expect(el.querySelectorAll("tbody tr").length).to.equal(3);
  });

  it("sorts ascending then descending on header click", async () => {
    const el = await fixture<DataTable>(
      html`<data-table .columns=${COLUMNS} .data=${DATA} .pageSize=${10}></data-table>`,
    );
    await el.updateComplete;
    const nameHeader = el.querySelector<HTMLElement>("th [data-sortable]")!;

    nameHeader.click();
    await el.updateComplete;
    expect(firstColumnText(el)).to.deep.equal(["a", "b", "c"]);

    nameHeader.click();
    await el.updateComplete;
    expect(firstColumnText(el)).to.deep.equal(["c", "b", "a"]);
  });

  it("paginates and exposes prev/next", async () => {
    const el = await fixture<DataTable>(
      html`<data-table .columns=${COLUMNS} .data=${DATA} .pageSize=${2}></data-table>`,
    );
    await el.updateComplete;
    expect(el.querySelectorAll("tbody tr").length).to.equal(2);
    expect(el.textContent).to.contain("1 / 2");

    const next = Array.from(el.querySelectorAll<HTMLButtonElement>("button")).find(
      (b) => b.textContent?.includes("Next"),
    )!;
    next.click();
    await el.updateComplete;
    expect(el.querySelectorAll("tbody tr").length).to.equal(1);
    expect(el.textContent).to.contain("2 / 2");
  });
});
