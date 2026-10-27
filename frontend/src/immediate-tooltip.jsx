import { useId, useLayoutEffect, useRef, useState } from "react";
import { createPortal, flushSync } from "react-dom";

export function ImmediateTooltip({ text, focusable = false, children }) {
  return <span className="immediate-tooltip-trigger" data-tooltip={text || undefined}
    tabIndex={focusable && text ? 0 : undefined}>{children}</span>;
}

// Delegate to one floating tooltip, including dynamically added scan cells and
// modal content. Native elements keep their layout, focus and click behavior.
export function ImmediateTooltipProvider({ children }) {
  const id = useId();
  const tooltipRef = useRef(null);
  const activeRef = useRef(null);
  const [hint, setHint] = useState(null);

  useLayoutEffect(() => {
    let hovered = null;
    let focused = null;
    let dismissed = null;
    const targetFor = (node) => node instanceof Element ? node.closest("[data-tooltip]") : null;
    const insideTooltip = (node) => node instanceof Node && tooltipRef.current?.contains(node);
    const show = (target) => {
      const text = target?.isConnected ? target.getAttribute("data-tooltip") : "";
      const next = text && target !== dismissed ? { target, text } : null;
      if (activeRef.current?.target === next?.target && activeRef.current?.text === next?.text) return;
      activeRef.current = next;
      setHint(next);
    };
    const over = (event) => {
      if (insideTooltip(event.target)) return;
      const next = targetFor(event.target);
      if (hovered !== next) dismissed = null;
      hovered = next;
      flushSync(() => show(hovered || focused));
    };
    const out = (event) => {
      if (insideTooltip(event.relatedTarget)) return;
      const next = targetFor(event.relatedTarget);
      if (hovered !== next) dismissed = null;
      hovered = next;
      flushSync(() => show(hovered || focused));
    };
    const focus = (event) => {
      focused = targetFor(event.target);
      dismissed = null;
      show(focused);
    };
    const blur = (event) => {
      focused = targetFor(event.relatedTarget);
      if (dismissed !== focused) dismissed = null;
      show(hovered || focused);
    };
    const dismiss = () => {
      dismissed = activeRef.current?.target;
      show(null);
    };
    const escape = (event) => {
      if (event.key !== "Escape" || !activeRef.current) return;
      event.preventDefault();
      event.stopPropagation();
      dismiss();
    };
    const click = (event) => { if (!insideTooltip(event.target)) dismiss(); };
    const listeners = { mouseover: over, mouseout: out, focusin: focus, focusout: blur, keydown: escape, click };
    for (const [name, handler] of Object.entries(listeners)) document.addEventListener(name, handler, true);
    const close = () => { hovered = null; focused = null; show(null); };
    window.addEventListener("blur", close);
    return () => {
      for (const [name, handler] of Object.entries(listeners)) document.removeEventListener(name, handler, true);
      window.removeEventListener("blur", close);
    };
  }, []);

  useLayoutEffect(() => {
    if (!hint) return undefined;
    const tooltip = tooltipRef.current;
    // The top layer keeps tooltips above native modal dialogs and clipping
    // containers. Portal into the current dialog to retain modal interaction.
    tooltip.showPopover?.();
    const place = () => {
      if (!hint.target.isConnected) {
        activeRef.current = null;
        setHint(null);
        return;
      }
      const rect = hint.target.getBoundingClientRect();
      const { width, height } = tooltip.getBoundingClientRect();
      tooltip.style.left = `${Math.max(8, Math.min(rect.left, window.innerWidth - width - 8))}px`;
      const top = rect.bottom + height <= window.innerHeight - 8 ? rect.bottom : rect.top - height;
      tooltip.style.top = `${Math.max(8, Math.min(top, window.innerHeight - height - 8))}px`;
    };
    place();
    const control = hint.target instanceof HTMLLabelElement ? hint.target.control
      : hint.target.classList.contains("immediate-tooltip-trigger") ? hint.target.querySelector("button, input, select") : null;
    const described = [...new Set([hint.target, control].filter(Boolean))];
    for (const element of described) {
      const ids = (element.getAttribute("aria-describedby") || "").split(/\s+/).filter(Boolean);
      element.setAttribute("aria-describedby", [...new Set([...ids, id])].join(" "));
    }
    const observer = new MutationObserver(() => {
      const text = hint.target.getAttribute("data-tooltip");
      const next = text ? { target: hint.target, text } : null;
      activeRef.current = next;
      setHint(next);
    });
    observer.observe(hint.target, { attributes: true, attributeFilter: ["data-tooltip"] });
    window.addEventListener("resize", place);
    window.addEventListener("scroll", place, true);
    return () => {
      observer.disconnect();
      for (const element of described) {
        const remaining = (element.getAttribute("aria-describedby") || "").split(/\s+/).filter((value) => value && value !== id);
        if (remaining.length) element.setAttribute("aria-describedby", remaining.join(" "));
        else element.removeAttribute("aria-describedby");
      }
      window.removeEventListener("resize", place);
      window.removeEventListener("scroll", place, true);
    };
  }, [hint, id]);

  const host = hint?.target.closest("dialog[open]") || document.body;
  return <>{children}{hint && createPortal(
    <span ref={tooltipRef} id={id} role="tooltip" popover="manual" className="immediate-tooltip">
      <span className="immediate-tooltip-content">{hint.text}</span>
    </span>, host
  )}</>;
}
